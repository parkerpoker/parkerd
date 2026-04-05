package meshruntime

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
)

// v3 session-transport integration for meshRuntime.
//
// The session manager is initialized lazily on first v3 exchange. Discovery
// (peer.manifest.get) stays on the v2 one-shot path. All other RPCs attempt
// v3 when the peer advertises session-transport-v3, falling back to v2.

const (
	v3ReadTimeout  = 120 * time.Second // idle read deadline on server-side v3 sessions
	v3WriteTimeout = 20 * time.Second
)

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
	// The peer info doesn't directly expose capabilities, but we can check the
	// TransportPubkeyHex (required for v3) and rely on the wire version from exchange.
	// For now, we check if the peer explicitly includes the capability in its manifest.
	// The caller should check TransportWireVersion from the cached manifest.
	return strings.TrimSpace(info.TransportPubkeyHex) != ""
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
	resp, err := sm.Request(peerURL, pubBytes, request)
	if err != nil {
		if runtime.sessionMetrics != nil {
			runtime.sessionMetrics.RequestTimeoutCount.Add(1)
		}
		return transportpkg.TransportEnvelope{}, err
	}
	return resp, nil
}

// exchangePeerTransportAuto attempts v3 if the peer supports it, otherwise uses v2.
func (runtime *meshRuntime) exchangePeerTransportAuto(peerURL string, peerInfo nativePeerSelf, request transportpkg.TransportEnvelope) (transportpkg.TransportEnvelope, error) {
	if peerSupportsV3(peerInfo) {
		resp, err := runtime.exchangePeerTransportV3(peerURL, peerInfo, request)
		if err == nil {
			return resp, nil
		}
		// On handshake or session error, fall back to v2.
		if transportpkg.IsSessionReset(err) || isHandshakeErr(err) {
			debugMeshf("v3 session failed for %s, falling back to v2: %v", peerURL, err)
			if runtime.sessionMetrics != nil {
				runtime.sessionMetrics.FallbackToV2Count.Add(1)
			}
			return runtime.exchangePeerTransport(peerURL, request)
		}
		return transportpkg.TransportEnvelope{}, err
	}
	return runtime.exchangePeerTransport(peerURL, request)
}

func isHandshakeErr(err error) bool {
	var te *transportpkg.TransportError
	return errors.As(err, &te) && te.Kind == transportpkg.ErrKindHandshakeFailed
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
	defer conn.Close()

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
	for {
		request, err := transportpkg.ReadV3Request(conn, noise, v3ReadTimeout)
		if err != nil {
			if err != io.EOF {
				debugMeshf("v3 read error: %v", err)
			}
			return
		}

		// Ignore keepalive pings.
		if request.MessageType == "keepalive" {
			pong := transportpkg.TransportEnvelope{
				MessageType: "keepalive",
				MessageID:   request.MessageID,
				RetryOf:     request.MessageID,
			}
			if err := transportpkg.WriteV3Response(conn, noise, pong, v3WriteTimeout); err != nil {
				return
			}
			continue
		}

		response, handleErr := runtime.handlePeerTransportEnvelope(request)
		if handleErr != nil {
			response = runtime.nackFromRequest(request, handleErr)
		}
		// Ensure RetryOf is set so the client can correlate the response.
		if response.RetryOf == "" {
			response.RetryOf = request.MessageID
		}

		if err := transportpkg.WriteV3Response(conn, noise, response, v3WriteTimeout); err != nil {
			return
		}
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
