package meshruntime

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
)

// v3 session-transport integration for meshRuntime.
//
// The session manager is initialized lazily on first v3 exchange. Discovery
// (peer.manifest.get) stays on the v2 one-shot path. All other RPCs attempt
// v3 when the peer advertises session-transport-v3, falling back to v2.

const maxV3SessionConcurrentHandlers = 8

type peerTransportRequestBuilder func(peerInfo nativePeerSelf) (transportpkg.TransportEnvelope, string, error)

// ensureSessionManager lazily creates the outbound session manager.
func (runtime *meshRuntime) ensureSessionManager() *transportpkg.SessionManager {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.sessionManager != nil {
		return runtime.sessionManager
	}
	isTor := runtime.config.UseTor
	cfg := transportpkg.ResolveSessionConfig(isTor)
	if runtime.sessionMetrics == nil {
		runtime.sessionMetrics = &transportpkg.SessionMetrics{}
	}
	runtime.sessionManager = transportpkg.NewSessionManager(cfg, runtime.sessionMetrics, runtime.dialPeerTransport)
	return runtime.sessionManager
}

// peerSupportsV3 checks whether the peer manifest advertises session-transport-v3.
func peerSupportsV3(info nativePeerSelf) bool {
	return info.TransportWireVersion >= 3 && strings.TrimSpace(info.TransportPubkeyHex) != ""
}

// peerStaticPubBytes returns the 32-byte X25519 public key from the peer info.
func peerStaticPubBytes(info nativePeerSelf) ([]byte, error) {
	pubHex := strings.TrimSpace(info.TransportPubkeyHex)
	if pubHex == "" {
		return nil, fmt.Errorf("peer has no transport public key")
	}
	return hex.DecodeString(pubHex)
}

// exchangePeerTransportV3 sends a request over a persistent v3 session and returns
// the response. Falls back to v2 if the session fails and fallbackV2 is true.
func (runtime *meshRuntime) exchangePeerTransportV3(peerURL string, peerInfo nativePeerSelf, request transportpkg.TransportEnvelope) (transportpkg.TransportEnvelope, error) {
	pubBytes, err := peerStaticPubBytes(peerInfo)
	if err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	sm := runtime.ensureSessionManager()
	request, err = runtime.signEnvelopeForWireVersion(request, transportpkg.WireVersion)
	if err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	resp, err := sm.Request(peerURL, pubBytes, request)
	if err != nil {
		if runtime.sessionMetrics != nil && transportpkg.IsTransportTimeout(err) {
			runtime.sessionMetrics.RequestTimeoutCount.Add(1)
		}
		return transportpkg.TransportEnvelope{}, err
	}
	if resp.MessageType != "keepalive" && resp.TransportWireVersion != transportpkg.WireVersion {
		runtime.invalidateSessionForPeer(peerURL)
		return transportpkg.TransportEnvelope{}, &transportpkg.TransportError{
			Kind:    transportpkg.ErrKindWireDowngrade,
			PeerURL: peerURL,
			Detail:  fmt.Sprintf("expected wire version %d, got %d", transportpkg.WireVersion, resp.TransportWireVersion),
		}
	}
	return resp, nil
}

// exchangePeerTransportAuto attempts v3 if the peer supports it, otherwise uses v2.
// On retry-worthy v3 errors it refreshes discovery, rebuilds the request, and
// retries v3 first when the peer still advertises v3 support. Only peers that no
// longer advertise v3 are downgraded to the v2 one-shot path.
func (runtime *meshRuntime) exchangePeerTransportAuto(peerURL string, buildRequest peerTransportRequestBuilder) (transportpkg.TransportEnvelope, string, error) {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return transportpkg.TransportEnvelope{}, "", err
	}
	request, requestKey, err := buildRequest(peerInfo)
	if err != nil {
		return transportpkg.TransportEnvelope{}, "", err
	}
	if peerSupportsV3(peerInfo) {
		resp, err := runtime.exchangePeerTransportV3(peerURL, peerInfo, request)
		if err == nil {
			return resp, requestKey, nil
		}
		if shouldRefreshV3Exchange(err) {
			refreshedPeerInfo, refreshErr := runtime.fetchPeerInfoFresh(peerURL)
			if refreshErr != nil {
				return transportpkg.TransportEnvelope{}, "", errors.Join(err, fmt.Errorf("refresh peer manifest: %w", refreshErr))
			}
			retryRequest, retryKey, buildErr := buildRequest(refreshedPeerInfo)
			if buildErr != nil {
				return transportpkg.TransportEnvelope{}, "", errors.Join(err, fmt.Errorf("rebuild refreshed request: %w", buildErr))
			}
			if peerSupportsV3(refreshedPeerInfo) {
				if transportpkg.IsWireDowngrade(err) {
					return transportpkg.TransportEnvelope{}, "", err
				}
				debugMeshf("v3 session failed for %s, retrying with refreshed v3 manifest: %v", peerURL, err)
				resp, retryErr := runtime.exchangePeerTransportV3(peerURL, refreshedPeerInfo, retryRequest)
				if retryErr == nil {
					return resp, retryKey, nil
				}
				if transportpkg.IsWireDowngrade(retryErr) || !isRetryableV3SessionErr(retryErr) {
					return transportpkg.TransportEnvelope{}, "", retryErr
				}
				err = retryErr
			}
			debugMeshf("v3 session failed for %s, falling back to v2: %v", peerURL, err)
			if runtime.sessionMetrics != nil {
				runtime.sessionMetrics.FallbackToV2Count.Add(1)
			}
			resp, fallbackErr := runtime.exchangePeerTransport(peerURL, retryRequest)
			if fallbackErr != nil {
				return transportpkg.TransportEnvelope{}, "", fallbackErr
			}
			return resp, retryKey, nil
		}
		return transportpkg.TransportEnvelope{}, "", err
	}
	resp, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return transportpkg.TransportEnvelope{}, "", err
	}
	return resp, requestKey, nil
}

func isHandshakeErr(err error) bool {
	var te *transportpkg.TransportError
	return errors.As(err, &te) && te.Kind == transportpkg.ErrKindHandshakeFailed
}

func isRetryableV3SessionErr(err error) bool {
	return transportpkg.IsSessionReset(err) || transportpkg.IsTransportTimeout(err) || isHandshakeErr(err) || transportpkg.IsSessionClosed(err)
}

func shouldRefreshV3Exchange(err error) bool {
	return isRetryableV3SessionErr(err) || transportpkg.IsWireDowngrade(err)
}

// closeSessionManager tears down the session manager if active.
func (runtime *meshRuntime) closeSessionManager() {
	runtime.mu.Lock()
	sm := runtime.sessionManager
	runtime.sessionManager = nil
	runtime.mu.Unlock()
	if sm != nil {
		sm.CloseAll()
	}
}

// invalidateSessionForPeer closes a specific peer session, e.g. on key rotation.
func (runtime *meshRuntime) invalidateSessionForPeer(peerURL string) {
	runtime.mu.Lock()
	sm := runtime.sessionManager
	runtime.mu.Unlock()
	if sm != nil {
		sm.CloseSession(peerURL)
	}
}

// handleV3PeerTransportConnection handles an inbound v3 session connection.
// The v3 preface has already been consumed by the listener dispatch.
func (runtime *meshRuntime) handleV3PeerTransportConnection(conn net.Conn) {
	shutdownCh := runtime.shutdownSignal()
	var wg sync.WaitGroup
	defer func() {
		_ = conn.Close()
		wg.Wait()
	}()

	staticPrivBytes, err := hex.DecodeString(runtime.transportPrivate)
	if err != nil {
		return
	}
	staticPubBytes, err := hex.DecodeString(runtime.transportPublic)
	if err != nil {
		return
	}

	isTor := runtime.config.UseTor
	cfg := transportpkg.ResolveSessionConfig(isTor)
	noise, err := transportpkg.AcceptV3Session(conn, staticPrivBytes, staticPubBytes, cfg.HandshakeTimeout)
	if err != nil {
		debugMeshf("v3 handshake failed: %v", err)
		return
	}

	// Serve requests over the session until idle timeout or error.
	// Use config-driven timeouts so env knobs govern both client and server.
	// Requests are handled concurrently so a slow handler doesn't block
	// keepalive processing or unrelated RPCs on the same session.
	readTimeout := cfg.IdleTimeout
	writeTimeout := cfg.RequestTimeout
	handlerSlots := make(chan struct{}, maxV3SessionConcurrentHandlers)

	var writeMu sync.Mutex // serializes noise.Encrypt + conn.Write

	writeResponse := func(resp transportpkg.TransportEnvelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return transportpkg.WriteV3Response(conn, noise, resp, writeTimeout)
	}

	for {
		request, err := transportpkg.ReadV3Request(conn, noise, readTimeout)
		if err != nil {
			if err != io.EOF {
				debugMeshf("v3 read error: %v", err)
			}
			return
		}

		// Respond to keepalive pings inline (cheap, no handler work).
		if request.MessageType == "keepalive" {
			pong := transportpkg.TransportEnvelope{
				MessageType: "keepalive",
				MessageID:   request.MessageID,
				RetryOf:     request.MessageID,
			}
			if err := writeResponse(pong); err != nil {
				return
			}
			continue
		}
		if request.TransportWireVersion != transportpkg.WireVersion {
			downgrade := &transportpkg.TransportError{
				Kind:   transportpkg.ErrKindWireDowngrade,
				Detail: fmt.Sprintf("expected wire version %d, got %d", transportpkg.WireVersion, request.TransportWireVersion),
			}
			response, nackErr := runtime.signEnvelopeForWireVersion(runtime.plainNack(request.MessageID, downgrade), transportpkg.WireVersion)
			if nackErr != nil {
				return
			}
			if err := writeResponse(response); err != nil {
				return
			}
			continue
		}
		if !acquireV3HandlerSlot(handlerSlots, shutdownCh) {
			return
		}
		if !runtime.beginBackgroundTask() {
			<-handlerSlots
			return
		}
		wg.Add(1)
		go func(req transportpkg.TransportEnvelope) {
			defer wg.Done()
			defer runtime.endBackgroundTask()
			defer func() {
				<-handlerSlots
			}()
			response, handleErr := runtime.handlePeerTransportEnvelope(req)
			if handleErr != nil {
				response = runtime.nackFromRequest(req, handleErr)
			}
			if response.RetryOf == "" {
				response.RetryOf = req.MessageID
			}
			if err := writeResponse(response); err != nil {
				debugMeshf("v3 write error: %v", err)
			}
		}(request)
	}
}

func acquireV3HandlerSlot(handlerSlots chan struct{}, shutdownCh <-chan struct{}) bool {
	select {
	case handlerSlots <- struct{}{}:
		return true
	case <-shutdownCh:
		return false
	}
}

// SessionMetricsSnapshot returns a snapshot of session transport metrics, or nil if not initialized.
func (runtime *meshRuntime) SessionMetricsSnapshot() *transportpkg.SessionMetricsSnapshot {
	runtime.mu.Lock()
	m := runtime.sessionMetrics
	runtime.mu.Unlock()
	if m == nil {
		return nil
	}
	snap := m.Snapshot()
	return &snap
}

// peerManifestHasV3 checks if the peer info response contains v3 capability.
// We detect this by checking if the peer advertises TransportWireVersion >= 3 in their response.
func peerManifestHasV3(body json.RawMessage) bool {
	var peek struct {
		TransportWireVersion int `json:"transportWireVersion"`
	}
	if json.Unmarshal(body, &peek) == nil && peek.TransportWireVersion >= 3 {
		return true
	}
	return false
}
