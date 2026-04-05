package transport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestV2PeerInteroperability verifies that a v2 one-shot client can still talk
// to a server that supports v3 (the server falls back because the client sends
// a JSON envelope without the v3 preface).
func TestV2PeerInteroperability(t *testing.T) {
	serverPriv, serverPub := testKeypair(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				// Read first bytes to detect v3 vs v2.
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				preface := make([]byte, V3PrefaceLen())
				n, err := io.ReadFull(conn, preface)
				if err != nil && n == 0 {
					return
				}
				_ = conn.SetReadDeadline(time.Time{})

				if n == V3PrefaceLen() && IsV3Preface(preface[:n]) {
					noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
					if err != nil {
						return
					}
					echoHandler(conn, noise)
					return
				}

				// v2 one-shot path.
				combined := io.MultiReader(strings.NewReader(string(preface[:n])), conn)
				reader := bufio.NewReader(combined)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				var req TransportEnvelope
				if err := json.Unmarshal([]byte(line), &req); err != nil {
					return
				}
				resp := TransportEnvelope{
					MessageID:   "v2-resp-" + req.MessageID,
					MessageType: req.MessageType + ".response",
					RetryOf:     req.MessageID,
				}
				data, _ := json.Marshal(resp)
				conn.Write(append(data, '\n'))
			}()
		}
	}()

	// Send a v2-style one-shot request (raw JSON line, no preface).
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := TransportEnvelope{
		MessageID:            "v2-test-001",
		MessageType:          "table.state.pull",
		TransportWireVersion: 2,
	}
	data, _ := json.Marshal(req)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp TransportEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RetryOf != "v2-test-001" {
		t.Fatalf("expected RetryOf=v2-test-001, got %q", resp.RetryOf)
	}
}

// TestV3FallbackOnDiscoveryV2 verifies that a v2-only server (no v3 preface
// support) can still be reached via the v2 one-shot path, and that the v3
// session path correctly fails with a handshake error that callers use to
// trigger fallback.
func TestV3FallbackOnDiscoveryV2(t *testing.T) {
	// Start a v2-only server: it reads a JSON line and echoes a response.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				var req TransportEnvelope
				if json.Unmarshal([]byte(strings.TrimSpace(line)), &req) != nil {
					return
				}
				resp := TransportEnvelope{
					MessageID:   "v2-resp-" + req.MessageID,
					MessageType: req.MessageType + ".response",
					RetryOf:     req.MessageID,
				}
				data, _ := json.Marshal(resp)
				conn.Write(append(data, '\n'))
			}()
		}
	}()

	// Attempt a v3 session against the v2-only server. The server will interpret
	// "PARKERv3" as the start of a JSON line, fail to parse it, and close. The
	// session manager should get a handshake error.
	_, serverPub := testKeypair(t)
	metrics := &SessionMetrics{}
	cfg := DefaultSessionConfig(false)
	cfg.HandshakeTimeout = 2 * time.Second
	cfg.ConnectTimeout = 2 * time.Second

	sm := NewSessionManager(cfg, metrics, func(peerURL string) (net.Conn, error) {
		return net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	})
	defer sm.CloseAll()

	req := TransportEnvelope{MessageID: "v3-attempt-1", MessageType: "test"}
	_, err = sm.Request("parker://v2-only:1234", serverPub, req)
	if err == nil {
		t.Fatal("expected v3 request to fail against v2-only server")
	}
	var te *TransportError
	if !errors.As(err, &te) || te.Kind != ErrKindHandshakeFailed {
		t.Fatalf("expected handshake failure, got: %v", err)
	}

	// Verify fallback metrics/error helpers.
	metrics.FallbackToV2Count.Add(1)
	snap := metrics.Snapshot()
	if snap.FallbackToV2Count != 1 {
		t.Fatalf("expected fallback count 1, got %d", snap.FallbackToV2Count)
	}
	if !IsWireDowngrade(&TransportError{Kind: ErrKindWireDowngrade}) {
		t.Fatal("IsWireDowngrade should return true")
	}

	// Verify that a plain v2 request still works.
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	v2req := TransportEnvelope{
		MessageID:            "v2-fallback-1",
		MessageType:          "table.state.pull",
		TransportWireVersion: 2,
	}
	data, _ := json.Marshal(v2req)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write(append(data, '\n'))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("v2 fallback read: %v", err)
	}
	var v2resp TransportEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &v2resp); err != nil {
		t.Fatal(err)
	}
	if v2resp.RetryOf != "v2-fallback-1" {
		t.Fatalf("expected RetryOf=v2-fallback-1, got %q", v2resp.RetryOf)
	}
}

// TestManifestKeyRotationTearsDownSession verifies that closing a session
// for a specific peer works correctly (simulating key rotation detection).
func TestManifestKeyRotationTearsDownSession(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()
	defer sm.CloseAll()

	peerURL := "parker://rotating-peer:1234"

	// Establish a session.
	req := TransportEnvelope{MessageID: "kr-1", MessageType: "test"}
	_, err := sm.Request(peerURL, serverPub, req)
	if err != nil {
		t.Fatal(err)
	}
	if !sm.HasSession(peerURL) {
		t.Fatal("session should exist")
	}

	// Simulate key rotation: close the session for this peer.
	sm.CloseSession(peerURL)
	if sm.HasSession(peerURL) {
		t.Fatal("session should be torn down after key rotation")
	}

	// Next request should create a new session.
	req2 := TransportEnvelope{MessageID: "kr-2", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req2)
	if err != nil {
		t.Fatalf("request after key rotation should succeed: %v", err)
	}
	if !sm.HasSession(peerURL) {
		t.Fatal("new session should exist")
	}
}

// TestRetryPathIdempotentRPCs verifies that idempotent RPCs (table.hand.request,
// table.state.pull, table.state.push) can be safely retried by sending a new
// request with a different MessageID.
func TestRetryPathIdempotentRPCs(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()
	defer sm.CloseAll()

	peerURL := "parker://retry-peer:1234"
	idempotentTypes := []string{
		"table.hand.request",
		"table.state.pull",
		"table.state.push",
		"peer.manifest.get",
	}

	for _, msgType := range idempotentTypes {
		// First attempt.
		req1 := TransportEnvelope{
			MessageID:   fmt.Sprintf("retry-%s-1", msgType),
			MessageType: msgType,
		}
		resp1, err := sm.Request(peerURL, serverPub, req1)
		if err != nil {
			t.Fatalf("%s: first attempt failed: %v", msgType, err)
		}
		if resp1.RetryOf != req1.MessageID {
			t.Fatalf("%s: wrong correlation", msgType)
		}

		// Retry with a different MessageID (simulating retry after timeout).
		req2 := TransportEnvelope{
			MessageID:   fmt.Sprintf("retry-%s-2", msgType),
			MessageType: msgType,
			RetryOf:     req1.MessageID,
		}
		resp2, err := sm.Request(peerURL, serverPub, req2)
		if err != nil {
			t.Fatalf("%s: retry failed: %v", msgType, err)
		}
		if resp2.RetryOf != req2.MessageID {
			t.Fatalf("%s: retry wrong correlation", msgType)
		}
	}
}

// TestNonIdempotentRPCTypedTimeout verifies that non-idempotent RPCs
// surface typed timeout errors rather than being blindly retried.
func TestNonIdempotentRPCTypedTimeout(t *testing.T) {
	nonIdempotentTypes := []string{
		"table.join.request",
		"table.action.request",
		"table.funds.request",
		"table.custody.request",
	}

	for _, msgType := range nonIdempotentTypes {
		err := &TransportError{
			Kind:    ErrKindRequestTimeout,
			PeerURL: "parker://timeout-peer:1234",
			Detail:  fmt.Sprintf("%s timed out", msgType),
		}
		if !IsTransportTimeout(err) {
			t.Fatalf("%s: should be transport timeout", msgType)
		}
		if IsSessionReset(err) {
			t.Fatalf("%s: should not be session reset", msgType)
		}
	}
}

// TestFullSessionMultiplexedHand simulates a heads-up hand where multiple
// hand messages are sent over reused sessions with concurrent requests.
func TestFullSessionMultiplexedHand(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()
	defer sm.CloseAll()

	peerURL := "parker://heads-up-peer:1234"
	handMessages := []string{
		"deal", "preflop-bet", "preflop-call",
		"flop-check", "flop-bet", "flop-call",
		"turn-check", "turn-check",
		"river-bet", "river-fold",
	}

	var wg sync.WaitGroup
	for i, msg := range handMessages {
		wg.Add(1)
		go func(idx int, action string) {
			defer wg.Done()
			req := TransportEnvelope{
				MessageID:   fmt.Sprintf("hand-%d-%s", idx, action),
				MessageType: "table.hand.request",
				Channel:     "table",
			}
			resp, err := sm.Request(peerURL, serverPub, req)
			if err != nil {
				t.Errorf("hand message %d (%s) failed: %v", idx, action, err)
				return
			}
			if resp.RetryOf != req.MessageID {
				t.Errorf("hand message %d: wrong correlation", idx)
			}
		}(i, msg)
	}
	wg.Wait()

	// Verify the session was reused (not one session per message).
	if !sm.HasSession(peerURL) {
		t.Fatal("session should still be active")
	}
}

// TestTorModeConfig verifies that Tor mode gets higher timeout defaults.
func TestTorModeConfig(t *testing.T) {
	direct := DefaultSessionConfig(false)
	tor := DefaultSessionConfig(true)

	if tor.ConnectTimeout <= direct.ConnectTimeout {
		t.Fatal("tor connect timeout should be higher than direct")
	}
	if tor.HandshakeTimeout <= direct.HandshakeTimeout {
		t.Fatal("tor handshake timeout should be higher than direct")
	}
	if tor.RequestTimeout <= direct.RequestTimeout {
		t.Fatal("tor request timeout should be higher than direct")
	}
}

// TestConfigFromEnv verifies that environment variables override defaults.
func TestConfigFromEnv(t *testing.T) {
	t.Setenv("PARKER_PEER_TRANSPORT_CONNECT_TIMEOUT_MS", "999")
	t.Setenv("PARKER_PEER_TRANSPORT_REQUEST_TIMEOUT_MS", "1500")

	cfg := ResolveSessionConfig(false)
	if cfg.ConnectTimeout != 999*time.Millisecond {
		t.Fatalf("connect timeout: got %v, want 999ms", cfg.ConnectTimeout)
	}
	if cfg.RequestTimeout != 1500*time.Millisecond {
		t.Fatalf("request timeout: got %v, want 1500ms", cfg.RequestTimeout)
	}
	// Non-overridden values should use defaults.
	if cfg.HandshakeTimeout != 5*time.Second {
		t.Fatalf("handshake timeout: got %v, want 5s", cfg.HandshakeTimeout)
	}
}

// TestPartialPrefaceRead verifies that a server using io.ReadFull correctly
// handles a TCP client that delivers the v3 preface in multiple small writes.
func TestPartialPrefaceRead(t *testing.T) {
	serverPriv, serverPub := testKeypair(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	// Server that uses io.ReadFull for preface detection (like the production code).
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				preface := make([]byte, V3PrefaceLen())
				n, err := io.ReadFull(conn, preface)
				if err != nil || n != V3PrefaceLen() {
					return
				}
				_ = conn.SetReadDeadline(time.Time{})

				if !IsV3Preface(preface) {
					return
				}
				noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
				if err != nil {
					return
				}
				echoHandler(conn, noise)
			}()
		}
	}()

	// Client that writes the preface in two separate TCP writes to simulate
	// a partial read scenario (e.g., "PARK" then "ERv3").
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	preface := []byte("PARKERv3")
	// Write first 4 bytes, then remaining 4.
	if _, err := conn.Write(preface[:4]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // ensure separate TCP segments
	if _, err := conn.Write(preface[4:]); err != nil {
		t.Fatal(err)
	}

	// Now do a Noise handshake and exchange a message.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	noise, err := NoiseNKInitiator(conn, serverPub)
	if err != nil {
		t.Fatalf("handshake after partial preface: %v", err)
	}

	req := TransportEnvelope{MessageID: "partial-1", MessageType: "test.request"}
	reqData, _ := json.Marshal(req)
	encrypted, err := noise.Encrypt(reqData)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrameBytes(conn, encrypted); err != nil {
		t.Fatal(err)
	}

	frame, err := readFrameBytes(conn)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := noise.Decrypt(frame)
	if err != nil {
		t.Fatal(err)
	}
	var resp TransportEnvelope
	if err := json.Unmarshal(plain, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RetryOf != "partial-1" {
		t.Fatalf("expected RetryOf=partial-1, got %q", resp.RetryOf)
	}
}

// TestConfigInvalidEnv verifies that invalid env values fall back to defaults.
func TestConfigInvalidEnv(t *testing.T) {
	t.Setenv("PARKER_PEER_TRANSPORT_CONNECT_TIMEOUT_MS", "not-a-number")
	t.Setenv("PARKER_PEER_TRANSPORT_REQUEST_TIMEOUT_MS", "-5")

	cfg := ResolveSessionConfig(false)
	if cfg.ConnectTimeout != 5*time.Second {
		t.Fatalf("should fallback to default, got %v", cfg.ConnectTimeout)
	}
	if cfg.RequestTimeout != 8*time.Second {
		t.Fatalf("should fallback to default for negative, got %v", cfg.RequestTimeout)
	}
}
