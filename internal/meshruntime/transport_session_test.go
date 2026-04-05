package meshruntime

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
)

func closeSessionManagersForTest(t *testing.T, runtimes ...*meshRuntime) {
	t.Helper()
	t.Cleanup(func() {
		for _, runtime := range runtimes {
			if runtime == nil {
				continue
			}
			runtime.closeSessionManager()
		}
	})
}

func rotateTransportKeyForTest(t *testing.T, runtime *meshRuntime) string {
	t.Helper()

	privateKey, err := randomX25519PrivateKeyHex()
	if err != nil {
		t.Fatalf("generate rotated transport private key: %v", err)
	}
	publicKey, err := x25519PublicKeyHex(privateKey)
	if err != nil {
		t.Fatalf("derive rotated transport public key: %v", err)
	}
	runtime.transportPrivate = privateKey
	runtime.transportPublic = publicKey
	runtime.transportKeyID = transportKeyID(publicKey)
	runtime.self.TransportPubkeyHex = publicKey
	return publicKey
}

func backgroundTaskCountForTest(runtime *meshRuntime) int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.backgroundTasks
}

type closeNotifyConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (conn *closeNotifyConn) Close() error {
	err := conn.Conn.Close()
	conn.once.Do(func() {
		close(conn.closed)
	})
	return err
}

func TestExchangePeerTransportV3UsesVersion3EndToEnd(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	peerInfo, err := guest.fetchPeerInfo(host.selfPeerURL())
	if err != nil {
		t.Fatalf("fetch host peer info: %v", err)
	}
	request, requestKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build v3 peer probe: %v", err)
	}

	response, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, request)
	if err != nil {
		t.Fatalf("send v3 peer probe: %v", err)
	}
	if response.TransportWireVersion != transportpkg.WireVersion {
		t.Fatalf("response wire version: got %d, want %d", response.TransportWireVersion, transportpkg.WireVersion)
	}

	body, err := guest.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		t.Fatalf("decode v3 peer probe response: %v", err)
	}
	var manifest nativePeerSelf
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("unmarshal peer manifest: %v", err)
	}
	if manifest.TransportWireVersion != transportpkg.WireVersion {
		t.Fatalf("manifest wire version: got %d, want %d", manifest.TransportWireVersion, transportpkg.WireVersion)
	}
}

func TestFetchPeerInfoUsesV2OneShotWireVersion(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	peerURL := "parker://" + listener.Addr().String()
	advertisedPeer := host.self
	advertisedPeer.Peer.PeerURL = peerURL

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		line, err := readTrimmedLine(bufio.NewReader(conn))
		if err != nil {
			serverErr <- err
			return
		}
		var request transportpkg.TransportEnvelope
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			serverErr <- err
			return
		}
		if request.MessageType != nativeTransportMessagePeerProbe {
			serverErr <- fmt.Errorf("unexpected discovery request type %q", request.MessageType)
			return
		}
		if request.TransportWireVersion != nativeTransportOneShotWireVersion {
			serverErr <- fmt.Errorf("discovery request wire version: got %d, want %d", request.TransportWireVersion, nativeTransportOneShotWireVersion)
			return
		}

		body, err := json.Marshal(advertisedPeer)
		if err != nil {
			serverErr <- err
			return
		}
		response, err := host.buildEnvelope(nativeTransportMessagePeerManifest, nativeTransportChannelDiscovery, "", "", body, nil, request.MessageID)
		if err != nil {
			serverErr <- err
			return
		}
		response, err = host.signEnvelopeForWireVersion(response, nativeTransportOneShotWireVersion)
		if err != nil {
			serverErr <- err
			return
		}
		data, err := json.Marshal(response)
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := conn.Write(append(data, '\n')); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	peerInfo, err := guest.fetchPeerInfo(peerURL)
	if err != nil {
		t.Fatalf("fetch peer info: %v", err)
	}
	if peerInfo.Peer.PeerURL != peerURL {
		t.Fatalf("peer url: got %q, want %q", peerInfo.Peer.PeerURL, peerURL)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("v2 discovery server: %v", err)
	}
}

func TestExchangePeerTransportV3RejectsWireDowngrade(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	staticPrivBytes, err := hex.DecodeString(host.transportPrivate)
	if err != nil {
		t.Fatalf("decode host transport private key: %v", err)
	}
	staticPubBytes, err := hex.DecodeString(host.transportPublic)
	if err != nil {
		t.Fatalf("decode host transport public key: %v", err)
	}

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		preface := make([]byte, transportpkg.V3PrefaceLen())
		if _, err := io.ReadFull(conn, preface); err != nil {
			serverErr <- err
			return
		}
		if !transportpkg.IsV3Preface(preface) {
			serverErr <- fmt.Errorf("unexpected v3 preface %q", string(preface))
			return
		}
		noise, err := transportpkg.AcceptV3Session(conn, staticPrivBytes, staticPubBytes, 5*time.Second)
		if err != nil {
			serverErr <- err
			return
		}
		request, err := transportpkg.ReadV3Request(conn, noise, 5*time.Second)
		if err != nil {
			serverErr <- err
			return
		}
		if request.TransportWireVersion != transportpkg.WireVersion {
			serverErr <- fmt.Errorf("request wire version: got %d, want %d", request.TransportWireVersion, transportpkg.WireVersion)
			return
		}

		body, err := json.Marshal(host.self)
		if err != nil {
			serverErr <- err
			return
		}
		// Intentionally sign the response as v2 to verify the client rejects the downgrade.
		response, err := host.buildEnvelope(nativeTransportMessagePeerManifest, nativeTransportChannelDiscovery, "", "", body, nil, request.MessageID)
		if err != nil {
			serverErr <- err
			return
		}
		response, err = host.signEnvelopeForWireVersion(response, nativeTransportOneShotWireVersion)
		if err != nil {
			serverErr <- err
			return
		}
		if err := transportpkg.WriteV3Response(conn, noise, response, 5*time.Second); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	request, _, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build v3 peer probe: %v", err)
	}

	peerURL := "parker://" + listener.Addr().String()
	_, err = guest.exchangePeerTransportV3(peerURL, host.self, request)
	if err == nil {
		t.Fatal("expected wire downgrade error")
	}
	if !transportpkg.IsWireDowngrade(err) {
		t.Fatalf("expected wire downgrade error, got %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("downgrade test server: %v", err)
	}
}

func TestExchangePeerTransportAutoFallsBackToV2WireVersionAfterManifestDowngrade(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	peerURL := "parker://" + listener.Addr().String()
	advertisedPeer := host.self
	advertisedPeer.Peer.PeerURL = peerURL
	downgradedPeer := advertisedPeer
	downgradedPeer.TransportWireVersion = nativeTransportOneShotWireVersion

	staticPrivBytes, err := hex.DecodeString(host.transportPrivate)
	if err != nil {
		t.Fatalf("decode host transport private key: %v", err)
	}
	staticPubBytes, err := hex.DecodeString(host.transportPublic)
	if err != nil {
		t.Fatalf("decode host transport public key: %v", err)
	}

	var manifestRequests atomic.Int32
	var v3Requests atomic.Int32
	var fallbackV2Requests atomic.Int32
	serverErr := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if err := func() error {
				defer conn.Close()

				preface := make([]byte, transportpkg.V3PrefaceLen())
				n, readErr := io.ReadFull(conn, preface)
				if readErr == nil && n == transportpkg.V3PrefaceLen() && transportpkg.IsV3Preface(preface) {
					noise, err := transportpkg.AcceptV3Session(conn, staticPrivBytes, staticPubBytes, 5*time.Second)
					if err != nil {
						return err
					}
					request, err := transportpkg.ReadV3Request(conn, noise, 5*time.Second)
					if err != nil {
						return err
					}
					v3Requests.Add(1)
					response, err := host.buildEnvelope(request.MessageType+".response", request.Channel, request.TableID, "", []byte(`{}`), nil, request.MessageID)
					if err != nil {
						return err
					}
					response, err = host.signEnvelopeForWireVersion(response, nativeTransportOneShotWireVersion)
					if err != nil {
						return err
					}
					return transportpkg.WriteV3Response(conn, noise, response, 5*time.Second)
				}

				reader := bufio.NewReader(io.MultiReader(strings.NewReader(string(preface[:n])), conn))
				line, err := readTrimmedLine(reader)
				if err != nil {
					return err
				}
				var request transportpkg.TransportEnvelope
				if err := json.Unmarshal([]byte(line), &request); err != nil {
					return err
				}
				if request.MessageType == nativeTransportMessagePeerProbe {
					manifestCount := manifestRequests.Add(1)
					manifest := advertisedPeer
					if manifestCount > 1 {
						manifest = downgradedPeer
					}
					body, err := json.Marshal(manifest)
					if err != nil {
						return err
					}
					response, err := host.buildEnvelope(nativeTransportMessagePeerManifest, nativeTransportChannelDiscovery, "", "", body, nil, request.MessageID)
					if err != nil {
						return err
					}
					response, err = host.signEnvelopeForWireVersion(response, nativeTransportOneShotWireVersion)
					if err != nil {
						return err
					}
					data, err := json.Marshal(response)
					if err != nil {
						return err
					}
					_, err = conn.Write(append(data, '\n'))
					return err
				}

				fallbackV2Requests.Add(1)
				if request.TransportWireVersion != nativeTransportOneShotWireVersion {
					return fmt.Errorf("fallback request wire version: got %d, want %d", request.TransportWireVersion, nativeTransportOneShotWireVersion)
				}
				response, err := host.buildEnvelope(request.MessageType+".response", request.Channel, request.TableID, "", []byte(`{}`), nil, request.MessageID)
				if err != nil {
					return err
				}
				response, err = host.signEnvelopeForWireVersion(response, nativeTransportOneShotWireVersion)
				if err != nil {
					return err
				}
				data, err := json.Marshal(response)
				if err != nil {
					return err
				}
				_, err = conn.Write(append(data, '\n'))
				return err
			}(); err != nil {
				select {
				case serverErr <- err:
				default:
				}
				return
			}
		}
	}()

	response, requestKey, err := guest.exchangePeerTransportAuto(peerURL, func(peerInfo nativePeerSelf) (transportpkg.TransportEnvelope, string, error) {
		return guest.newOutboundEnvelope("test.rpc", nativeTransportChannelTable, "table-1", peerInfo.Peer.PeerID, map[string]any{"ok": true}, "")
	})
	if err != nil {
		t.Fatalf("send auto request with downgrade fallback: %v", err)
	}
	if response.TransportWireVersion != nativeTransportOneShotWireVersion {
		t.Fatalf("fallback response wire version: got %d, want %d", response.TransportWireVersion, nativeTransportOneShotWireVersion)
	}
	if _, err := guest.decodeResponseEnvelope(response, requestKey); err != nil {
		t.Fatalf("decode fallback response: %v", err)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	<-serverDone
	select {
	case err := <-serverErr:
		t.Fatalf("downgrade fallback server: %v", err)
	default:
	}
	if v3Requests.Load() != 1 {
		t.Fatalf("expected one downgraded v3 attempt, got %d", v3Requests.Load())
	}
	if manifestRequests.Load() != 2 {
		t.Fatalf("expected initial and refreshed manifest fetches, got %d", manifestRequests.Load())
	}
	if fallbackV2Requests.Load() != 1 {
		t.Fatalf("expected exactly one v2 fallback request, got %d", fallbackV2Requests.Load())
	}
}

func TestExchangePeerTransportAutoRejectsDowngradeWhenRefreshStillAdvertisesV3(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	peerURL := "parker://" + listener.Addr().String()
	advertisedPeer := host.self
	advertisedPeer.Peer.PeerURL = peerURL

	staticPrivBytes, err := hex.DecodeString(host.transportPrivate)
	if err != nil {
		t.Fatalf("decode host transport private key: %v", err)
	}
	staticPubBytes, err := hex.DecodeString(host.transportPublic)
	if err != nil {
		t.Fatalf("decode host transport public key: %v", err)
	}

	var manifestRequests atomic.Int32
	var fallbackV2Requests atomic.Int32
	serverErr := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if err := func() error {
				defer conn.Close()

				preface := make([]byte, transportpkg.V3PrefaceLen())
				n, readErr := io.ReadFull(conn, preface)
				if readErr == nil && n == transportpkg.V3PrefaceLen() && transportpkg.IsV3Preface(preface) {
					noise, err := transportpkg.AcceptV3Session(conn, staticPrivBytes, staticPubBytes, 5*time.Second)
					if err != nil {
						return err
					}
					request, err := transportpkg.ReadV3Request(conn, noise, 5*time.Second)
					if err != nil {
						return err
					}
					response, err := host.buildEnvelope(request.MessageType+".response", request.Channel, request.TableID, "", []byte(`{}`), nil, request.MessageID)
					if err != nil {
						return err
					}
					response, err = host.signEnvelopeForWireVersion(response, 2)
					if err != nil {
						return err
					}
					if err := transportpkg.WriteV3Response(conn, noise, response, 5*time.Second); err != nil {
						return err
					}
					return nil
				}

				reader := bufio.NewReader(io.MultiReader(strings.NewReader(string(preface[:n])), conn))
				line, err := readTrimmedLine(reader)
				if err != nil {
					return err
				}
				var request transportpkg.TransportEnvelope
				if err := json.Unmarshal([]byte(line), &request); err != nil {
					return err
				}
				if request.MessageType == nativeTransportMessagePeerProbe {
					manifestRequests.Add(1)
					body, err := json.Marshal(advertisedPeer)
					if err != nil {
						return err
					}
					response, err := host.buildEnvelope(nativeTransportMessagePeerManifest, nativeTransportChannelDiscovery, "", "", body, nil, request.MessageID)
					if err != nil {
						return err
					}
					data, err := json.Marshal(response)
					if err != nil {
						return err
					}
					if _, err := conn.Write(append(data, '\n')); err != nil {
						return err
					}
					return nil
				}

				fallbackV2Requests.Add(1)
				body := []byte(`{}`)
				response, err := host.buildEnvelope(request.MessageType+".response", request.Channel, request.TableID, "", body, nil, request.MessageID)
				if err != nil {
					return err
				}
				data, err := json.Marshal(response)
				if err != nil {
					return err
				}
				if _, err := conn.Write(append(data, '\n')); err != nil {
					return err
				}
				return nil
			}(); err != nil {
				select {
				case serverErr <- err:
				default:
				}
				return
			}
		}
	}()

	_, _, err = guest.exchangePeerTransportAuto(peerURL, func(peerInfo nativePeerSelf) (transportpkg.TransportEnvelope, string, error) {
		return guest.newOutboundEnvelope("test.rpc", nativeTransportChannelTable, "table-1", peerInfo.Peer.PeerID, map[string]any{"ok": true}, "")
	})
	if err == nil {
		t.Fatal("expected wire downgrade error")
	}
	if !transportpkg.IsWireDowngrade(err) {
		t.Fatalf("expected wire downgrade error, got %v", err)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	<-serverDone
	select {
	case err := <-serverErr:
		t.Fatalf("downgrade auto server: %v", err)
	default:
	}
	if manifestRequests.Load() != 2 {
		t.Fatalf("expected initial and refreshed manifest fetches, got %d", manifestRequests.Load())
	}
	if fallbackV2Requests.Load() != 0 {
		t.Fatalf("expected downgrade to stop before v2 fallback, got %d unexpected v2 retries", fallbackV2Requests.Load())
	}
}

func TestExchangePeerTransportAutoDoesNotRefreshManifestOnV3RequestTimeout(t *testing.T) {
	t.Setenv("PARKER_PEER_TRANSPORT_REQUEST_TIMEOUT_MS", "200")

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	peerURL := "parker://" + listener.Addr().String()
	advertisedPeer := host.self
	advertisedPeer.Peer.PeerURL = peerURL

	staticPrivBytes, err := hex.DecodeString(host.transportPrivate)
	if err != nil {
		t.Fatalf("decode host transport private key: %v", err)
	}
	staticPubBytes, err := hex.DecodeString(host.transportPublic)
	if err != nil {
		t.Fatalf("decode host transport public key: %v", err)
	}

	var manifestRequests atomic.Int32
	serverErr := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if err := func() error {
				defer conn.Close()

				preface := make([]byte, transportpkg.V3PrefaceLen())
				n, readErr := io.ReadFull(conn, preface)
				if readErr == nil && n == transportpkg.V3PrefaceLen() && transportpkg.IsV3Preface(preface) {
					noise, err := transportpkg.AcceptV3Session(conn, staticPrivBytes, staticPubBytes, 5*time.Second)
					if err != nil {
						return err
					}
					if _, err := transportpkg.ReadV3Request(conn, noise, 5*time.Second); err != nil {
						return err
					}
					time.Sleep(350 * time.Millisecond)
					return nil
				}

				reader := bufio.NewReader(io.MultiReader(strings.NewReader(string(preface[:n])), conn))
				line, err := readTrimmedLine(reader)
				if err != nil {
					return err
				}
				var request transportpkg.TransportEnvelope
				if err := json.Unmarshal([]byte(line), &request); err != nil {
					return err
				}
				if request.MessageType != nativeTransportMessagePeerProbe {
					return fmt.Errorf("unexpected one-shot request after timeout %q", request.MessageType)
				}
				if manifestRequests.Add(1) > 1 {
					return nil
				}

				body, err := json.Marshal(advertisedPeer)
				if err != nil {
					return err
				}
				response, err := host.buildEnvelope(nativeTransportMessagePeerManifest, nativeTransportChannelDiscovery, "", "", body, nil, request.MessageID)
				if err != nil {
					return err
				}
				response, err = host.signEnvelopeForWireVersion(response, nativeTransportOneShotWireVersion)
				if err != nil {
					return err
				}
				data, err := json.Marshal(response)
				if err != nil {
					return err
				}
				_, err = conn.Write(append(data, '\n'))
				return err
			}(); err != nil {
				select {
				case serverErr <- err:
				default:
				}
				return
			}
		}
	}()

	_, _, err = guest.exchangePeerTransportAuto(peerURL, func(peerInfo nativePeerSelf) (transportpkg.TransportEnvelope, string, error) {
		return guest.newOutboundEnvelope("test.rpc", nativeTransportChannelTable, "table-1", peerInfo.Peer.PeerID, map[string]any{"ok": true}, "")
	})
	if err == nil {
		t.Fatal("expected transport timeout")
	}
	if !transportpkg.IsTransportTimeout(err) {
		t.Fatalf("expected transport timeout, got %v", err)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	<-serverDone
	select {
	case err := <-serverErr:
		t.Fatalf("timeout test server: %v", err)
	default:
	}
	if manifestRequests.Load() != 1 {
		t.Fatalf("expected only the initial manifest fetch, got %d requests", manifestRequests.Load())
	}
}

func TestHandleV3PeerTransportConnectionClosesConnOnLocalKeyDecodeFailure(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	closeSessionManagersForTest(t, host)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	wrapped := &closeNotifyConn{Conn: serverConn, closed: make(chan struct{})}
	host.transportPrivate = "not-hex"

	done := make(chan struct{})
	go func() {
		host.handleV3PeerTransportConnection(wrapped)
		close(done)
	}()

	select {
	case <-wrapped.closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected connection to close on local key decode failure")
	}

	<-done
}

func TestHandleV3PeerTransportConnectionClosesConnOnHandshakeFailure(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	closeSessionManagersForTest(t, host)

	serverConn, clientConn := net.Pipe()
	wrapped := &closeNotifyConn{Conn: serverConn, closed: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		host.handleV3PeerTransportConnection(wrapped)
		close(done)
	}()

	if err := clientConn.Close(); err != nil {
		t.Fatalf("close client side: %v", err)
	}

	select {
	case <-wrapped.closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected connection to close on handshake failure")
	}

	<-done
}

func TestAcquireV3HandlerSlotUnblocksOnShutdown(t *testing.T) {
	handlerSlots := make(chan struct{}, 1)
	handlerSlots <- struct{}{}
	shutdownCh := make(chan struct{})

	result := make(chan bool, 1)
	go func() {
		result <- acquireV3HandlerSlot(handlerSlots, shutdownCh)
	}()

	close(shutdownCh)

	select {
	case ok := <-result:
		if ok {
			t.Fatal("expected shutdown to abort handler slot acquisition")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected shutdown to unblock handler slot acquisition")
	}
}

func TestHandleV3PeerTransportConnectionDoesNotBlockConcurrentRequests(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	peerInfo, err := guest.fetchPeerInfo(host.selfPeerURL())
	if err != nil {
		t.Fatalf("fetch host peer info: %v", err)
	}

	setupRequest, setupKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build setup probe: %v", err)
	}
	setupResponse, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, setupRequest)
	if err != nil {
		t.Fatalf("send setup probe: %v", err)
	}
	if _, err := guest.decodeResponseEnvelope(setupResponse, setupKey); err != nil {
		t.Fatalf("decode setup probe response: %v", err)
	}

	slowBody, err := guest.buildTableFetchRequest(tableID)
	if err != nil {
		t.Fatalf("build slow fetch request body: %v", err)
	}
	slowRequest, slowKey, err := guest.newOutboundEnvelope(
		nativeTransportMessageTablePull,
		nativeTransportChannelTable,
		tableID,
		peerInfo.Peer.PeerID,
		slowBody,
		peerInfo.TransportPubkeyHex,
	)
	if err != nil {
		t.Fatalf("build slow fetch request: %v", err)
	}
	fastRequest, fastKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build fast probe request: %v", err)
	}

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	lockErr := make(chan error, 1)
	go func() {
		lockErr <- host.store.withTableLock(tableID, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
	}()
	<-lockHeld

	slowDone := make(chan error, 1)
	go func() {
		response, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, slowRequest)
		if err == nil {
			_, err = guest.decodeResponseEnvelope(response, slowKey)
		}
		slowDone <- err
	}()

	fastDone := make(chan error, 1)
	go func() {
		response, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, fastRequest)
		if err == nil {
			_, err = guest.decodeResponseEnvelope(response, fastKey)
		}
		fastDone <- err
	}()

	select {
	case err := <-fastDone:
		if err != nil {
			close(releaseLock)
			<-lockErr
			t.Fatalf("fast request failed: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		close(releaseLock)
		<-lockErr
		t.Fatal("fast request blocked behind slow request on the same v3 session")
	}

	select {
	case err := <-slowDone:
		close(releaseLock)
		<-lockErr
		t.Fatalf("slow request completed before releasing the table lock: %v", err)
	default:
	}

	close(releaseLock)
	if err := <-lockErr; err != nil {
		t.Fatalf("release table lock: %v", err)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow request failed after releasing the table lock: %v", err)
	}
}

func TestCachePeerInfoInvalidatesCanonicalSessionOnAliasKeyRotation(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	canonicalURL := host.selfPeerURL()
	aliasURL := strings.Replace(canonicalURL, "parker://", "tcp://", 1)
	if aliasURL == canonicalURL {
		t.Fatalf("expected alias URL to differ from canonical URL %q", canonicalURL)
	}

	peerInfo, err := guest.fetchPeerInfo(aliasURL)
	if err != nil {
		t.Fatalf("fetch host peer info through alias: %v", err)
	}
	request, requestKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build canonical v3 probe: %v", err)
	}
	response, err := guest.exchangePeerTransportV3(canonicalURL, peerInfo, request)
	if err != nil {
		t.Fatalf("send canonical v3 probe: %v", err)
	}
	if _, err := guest.decodeResponseEnvelope(response, requestKey); err != nil {
		t.Fatalf("decode canonical v3 probe response: %v", err)
	}

	sm := guest.ensureSessionManager()
	if !sm.HasSession(canonicalURL) {
		t.Fatalf("expected canonical session for %s to exist", canonicalURL)
	}

	rotated := peerInfo
	rotated.TransportPubkeyHex = guest.transportPublic
	guest.cachePeerInfo(aliasURL, rotated)

	if sm.HasSession(canonicalURL) {
		t.Fatalf("expected canonical session for %s to be invalidated by alias key rotation", canonicalURL)
	}
	if sm.HasSession(aliasURL) {
		t.Fatalf("expected alias session for %s to be invalidated by key rotation", aliasURL)
	}
}

func TestExchangePeerTransportAutoRefreshesManifestAndRetriesV3AfterKeyRotation(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	hostURL := host.selfPeerURL()

	staleInfo, err := guest.fetchPeerInfo(hostURL)
	if err != nil {
		t.Fatalf("fetch initial host peer info: %v", err)
	}
	guest.closeSessionManager()
	rotatedPub := rotateTransportKeyForTest(t, host)
	if rotatedPub == staleInfo.TransportPubkeyHex {
		t.Fatal("expected host transport key rotation to change the public key")
	}

	table, err := guest.fetchRemoteTable(hostURL, tableID)
	if err != nil {
		t.Fatalf("fetch remote table after key rotation: %v", err)
	}
	if table.Config.TableID != tableID {
		t.Fatalf("expected fetched table %s, got %s", tableID, table.Config.TableID)
	}

	cached, ok := guest.cachedPeerInfo(hostURL)
	if !ok {
		t.Fatal("expected refreshed peer info to be cached")
	}
	if cached.TransportPubkeyHex != rotatedPub {
		t.Fatalf("expected refreshed transport key %s, got %s", rotatedPub, cached.TransportPubkeyHex)
	}

	sm := guest.ensureSessionManager()
	if !sm.HasSession(hostURL) {
		t.Fatalf("expected refreshed v3 request to establish a session for %s", hostURL)
	}

	metrics := guest.SessionMetricsSnapshot()
	if metrics == nil {
		t.Fatal("expected session metrics after v3 retry")
	}
	if metrics.FallbackToV2Count != 0 {
		t.Fatalf("expected key rotation retry to stay on v3, got %d v2 fallbacks", metrics.FallbackToV2Count)
	}
}

func TestFetchPeerInfoRefreshInvalidatesExpiredSessionOnKeyRotation(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	hostURL := host.selfPeerURL()
	peerInfo, err := guest.fetchPeerInfo(hostURL)
	if err != nil {
		t.Fatalf("fetch host peer info: %v", err)
	}

	request, requestKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build v3 probe: %v", err)
	}
	response, err := guest.exchangePeerTransportV3(hostURL, peerInfo, request)
	if err != nil {
		t.Fatalf("send v3 probe: %v", err)
	}
	if _, err := guest.decodeResponseEnvelope(response, requestKey); err != nil {
		t.Fatalf("decode v3 probe response: %v", err)
	}

	sm := guest.ensureSessionManager()
	if !sm.HasSession(hostURL) {
		t.Fatalf("expected session for %s to exist", hostURL)
	}

	guest.peerInfoCache[hostURL] = nativeCachedPeerInfo{
		FetchedAt: time.Now().Add(-nativePeerInfoCacheTTL - time.Second),
		PeerSelf:  peerInfo,
	}
	rotatedPub := rotateTransportKeyForTest(t, host)

	refreshed, err := guest.fetchPeerInfo(hostURL)
	if err != nil {
		t.Fatalf("refresh expired peer info after key rotation: %v", err)
	}
	if refreshed.TransportPubkeyHex != rotatedPub {
		t.Fatalf("expected refreshed transport key %s, got %s", rotatedPub, refreshed.TransportPubkeyHex)
	}
	if sm.HasSession(hostURL) {
		t.Fatalf("expected expired-cache refresh to invalidate stale session for %s", hostURL)
	}
}

func TestCachePeerInfoInvalidatesAllAliasSessionsOnKeyRotation(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	canonicalURL := host.selfPeerURL()
	aliasOne := strings.Replace(canonicalURL, "parker://", "tcp://", 1)
	aliasTwo := canonicalURL + "?alias=second"

	peerInfo, err := guest.fetchPeerInfo(aliasOne)
	if err != nil {
		t.Fatalf("fetch host peer info through first alias: %v", err)
	}
	if _, err := guest.fetchPeerInfo(aliasTwo); err != nil {
		t.Fatalf("fetch host peer info through second alias: %v", err)
	}

	openSession := func(peerURL string) {
		t.Helper()
		request, requestKey, err := guest.newOutboundEnvelope(
			nativeTransportMessagePeerProbe,
			nativeTransportChannelDiscovery,
			"",
			"",
			map[string]any{},
			"",
		)
		if err != nil {
			t.Fatalf("build v3 probe for %s: %v", peerURL, err)
		}
		response, err := guest.exchangePeerTransportV3(peerURL, peerInfo, request)
		if err != nil {
			t.Fatalf("send v3 probe for %s: %v", peerURL, err)
		}
		if _, err := guest.decodeResponseEnvelope(response, requestKey); err != nil {
			t.Fatalf("decode v3 probe response for %s: %v", peerURL, err)
		}
	}

	openSession(canonicalURL)
	openSession(aliasOne)
	openSession(aliasTwo)

	sm := guest.ensureSessionManager()
	for _, peerURL := range []string{canonicalURL, aliasOne, aliasTwo} {
		if !sm.HasSession(peerURL) {
			t.Fatalf("expected session for %s to exist", peerURL)
		}
	}

	rotated := peerInfo
	rotated.TransportPubkeyHex = guest.transportPublic
	guest.cachePeerInfo(aliasOne, rotated)

	for _, peerURL := range []string{canonicalURL, aliasOne, aliasTwo} {
		if sm.HasSession(peerURL) {
			t.Fatalf("expected key rotation to invalidate session for %s", peerURL)
		}
	}
}

func TestHandleV3PeerTransportConnectionBoundsConcurrentHandlers(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	hostURL := host.selfPeerURL()

	peerInfo, err := guest.fetchPeerInfo(hostURL)
	if err != nil {
		t.Fatalf("fetch host peer info: %v", err)
	}

	setupRequest, setupKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build setup v3 probe: %v", err)
	}
	setupResponse, err := guest.exchangePeerTransportV3(hostURL, peerInfo, setupRequest)
	if err != nil {
		t.Fatalf("send setup v3 probe: %v", err)
	}
	if _, err := guest.decodeResponseEnvelope(setupResponse, setupKey); err != nil {
		t.Fatalf("decode setup v3 probe response: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for backgroundTaskCountForTest(host) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	baseline := backgroundTaskCountForTest(host)
	if baseline < 1 {
		t.Fatalf("expected active v3 session background task, got %d", baseline)
	}

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	lockErr := make(chan error, 1)
	go func() {
		lockErr <- host.store.withTableLock(tableID, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
	}()
	<-lockHeld

	requestCount := maxV3SessionConcurrentHandlers + 3
	results := make(chan error, requestCount)
	for i := 0; i < requestCount; i++ {
		go func() {
			requestBody, err := guest.buildTableFetchRequest(tableID)
			if err != nil {
				results <- err
				return
			}
			request, requestKey, err := guest.newOutboundEnvelope(
				nativeTransportMessageTablePull,
				nativeTransportChannelTable,
				tableID,
				peerInfo.Peer.PeerID,
				requestBody,
				peerInfo.TransportPubkeyHex,
			)
			if err != nil {
				results <- err
				return
			}
			response, err := guest.exchangePeerTransportV3(hostURL, peerInfo, request)
			if err == nil {
				_, err = guest.decodeResponseEnvelope(response, requestKey)
			}
			results <- err
		}()
	}

	expectedMax := baseline + maxV3SessionConcurrentHandlers
	deadline = time.Now().Add(2 * time.Second)
	for backgroundTaskCountForTest(host) < expectedMax && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := backgroundTaskCountForTest(host); got != expectedMax {
		close(releaseLock)
		<-lockErr
		t.Fatalf("expected background task count to saturate at %d, got %d", expectedMax, got)
	}

	close(releaseLock)
	if err := <-lockErr; err != nil {
		t.Fatalf("release table lock: %v", err)
	}
	for i := 0; i < requestCount; i++ {
		if err := <-results; err != nil {
			t.Fatalf("bounded concurrent request %d failed: %v", i, err)
		}
	}
}
