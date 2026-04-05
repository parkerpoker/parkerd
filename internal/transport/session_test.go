package transport

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testKeypair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key.Bytes(), key.PublicKey().Bytes()
}

// testSessionPair sets up a client SessionManager and a server goroutine that
// performs the v3 handshake and echoes requests with RetryOf set.
func testSessionPair(t *testing.T, serverHandler func(net.Conn, *NoiseSession)) (*SessionManager, []byte, func(string) (net.Conn, error), net.Listener) {
	t.Helper()
	serverPriv, serverPub := testKeypair(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				// Read v3 preface.
				preface := make([]byte, V3PrefaceLen())
				if _, err := io.ReadFull(conn, preface); err != nil {
					conn.Close()
					return
				}
				if !IsV3Preface(preface) {
					conn.Close()
					return
				}
				noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
				if err != nil {
					conn.Close()
					return
				}
				serverHandler(conn, noise)
			}()
		}
	}()

	dialer := func(peerURL string) (net.Conn, error) {
		return net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	}

	metrics := &SessionMetrics{}
	cfg := SessionConfig{
		ConnectTimeout:    2 * time.Second,
		HandshakeTimeout:  2 * time.Second,
		RequestTimeout:    3 * time.Second,
		IdleTimeout:       10 * time.Second,
		KeepaliveInterval: 60 * time.Second, // disable for tests
	}
	sm := NewSessionManager(cfg, metrics, dialer)
	return sm, serverPub, dialer, listener
}

func echoHandler(conn net.Conn, noise *NoiseSession) {
	defer conn.Close()
	for {
		req, err := ReadV3Request(conn, noise, 10*time.Second)
		if err != nil {
			return
		}
		if req.MessageType == "keepalive" {
			resp := TransportEnvelope{
				MessageType:          "keepalive",
				MessageID:            req.MessageID,
				RetryOf:              req.MessageID,
				TransportWireVersion: req.TransportWireVersion,
			}
			if err := WriteV3Response(conn, noise, resp, 5*time.Second); err != nil {
				return
			}
			continue
		}
		resp := TransportEnvelope{
			MessageType:          req.MessageType + ".response",
			MessageID:            "resp-" + req.MessageID,
			RetryOf:              req.MessageID,
			BodyCiphertext:       req.BodyCiphertext,
			TransportWireVersion: req.TransportWireVersion,
		}
		if err := WriteV3Response(conn, noise, resp, 5*time.Second); err != nil {
			return
		}
	}
}

func TestSessionRoundTrip(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()
	defer sm.CloseAll()

	req := TransportEnvelope{
		MessageID:   "req-001",
		MessageType: "test.request",
	}
	resp, err := sm.Request("parker://test-peer:1234", serverPub, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.RetryOf != "req-001" {
		t.Fatalf("expected RetryOf=req-001, got %q", resp.RetryOf)
	}
	if resp.MessageType != "test.request.response" {
		t.Fatalf("unexpected response type %q", resp.MessageType)
	}
}

func TestSessionReuse(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()
	defer sm.CloseAll()

	peerURL := "parker://reuse-peer:1234"

	// First request creates the session.
	req1 := TransportEnvelope{MessageID: "req-1", MessageType: "test"}
	_, err := sm.Request(peerURL, serverPub, req1)
	if err != nil {
		t.Fatal(err)
	}

	if !sm.HasSession(peerURL) {
		t.Fatal("expected session to exist after first request")
	}

	// Second request reuses it.
	req2 := TransportEnvelope{MessageID: "req-2", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req2)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSessionConcurrentRequests(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()
	defer sm.CloseAll()

	peerURL := "parker://concurrent-peer:1234"
	const numRequests = 20

	var wg sync.WaitGroup
	errors := make([]error, numRequests)
	responses := make([]TransportEnvelope, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := TransportEnvelope{
				MessageID:   fmt.Sprintf("concurrent-%d", idx),
				MessageType: "test",
			}
			resp, err := sm.Request(peerURL, serverPub, req)
			errors[idx] = err
			responses[idx] = resp
		}(i)
	}
	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("request %d failed: %v", i, err)
		}
	}

	// Verify each response correlates to the correct request.
	for i, resp := range responses {
		expected := fmt.Sprintf("concurrent-%d", i)
		if resp.RetryOf != expected {
			t.Errorf("response %d: RetryOf=%q, want %q", i, resp.RetryOf, expected)
		}
	}
}

func TestSessionTimeout(t *testing.T) {
	// Server handler that never responds.
	slowHandler := func(conn net.Conn, noise *NoiseSession) {
		defer conn.Close()
		// Read but never write back.
		for {
			_, err := ReadV3Request(conn, noise, 30*time.Second)
			if err != nil {
				return
			}
			// Intentionally don't respond.
			time.Sleep(30 * time.Second)
		}
	}

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
				preface := make([]byte, V3PrefaceLen())
				io.ReadFull(conn, preface)
				noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
				if err != nil {
					conn.Close()
					return
				}
				slowHandler(conn, noise)
			}()
		}
	}()

	metrics := &SessionMetrics{}
	cfg := SessionConfig{
		ConnectTimeout:    2 * time.Second,
		HandshakeTimeout:  2 * time.Second,
		RequestTimeout:    500 * time.Millisecond, // very short for test
		IdleTimeout:       10 * time.Second,
		KeepaliveInterval: 60 * time.Second,
	}
	sm := NewSessionManager(cfg, metrics, func(string) (net.Conn, error) {
		return net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	})
	defer sm.CloseAll()

	req := TransportEnvelope{MessageID: "timeout-req", MessageType: "test"}
	_, err = sm.Request("parker://slow-peer:1234", serverPub, req)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !IsTransportTimeout(err) {
		t.Fatalf("expected transport timeout error, got: %v", err)
	}
	if sm.HasSession("parker://slow-peer:1234") {
		t.Fatal("session should be torn down after timeout")
	}
}

func TestSessionTimeoutReconnectsImmediately(t *testing.T) {
	serverPriv, serverPub := testKeypair(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var connectionCount atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			index := connectionCount.Add(1)
			go func(conn net.Conn, index int32) {
				preface := make([]byte, V3PrefaceLen())
				if _, err := io.ReadFull(conn, preface); err != nil {
					conn.Close()
					return
				}
				noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
				if err != nil {
					conn.Close()
					return
				}
				if index == 1 {
					defer conn.Close()
					if _, err := ReadV3Request(conn, noise, 5*time.Second); err != nil {
						return
					}
					// Hold the first connection open past the client timeout.
					time.Sleep(750 * time.Millisecond)
					return
				}
				echoHandler(conn, noise)
			}(conn, index)
		}
	}()

	metrics := &SessionMetrics{}
	cfg := SessionConfig{
		ConnectTimeout:    2 * time.Second,
		HandshakeTimeout:  2 * time.Second,
		RequestTimeout:    200 * time.Millisecond,
		IdleTimeout:       5 * time.Second,
		KeepaliveInterval: 60 * time.Second,
	}
	sm := NewSessionManager(cfg, metrics, func(string) (net.Conn, error) {
		return net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	})
	defer sm.CloseAll()

	peerURL := "parker://timeout-reconnect-peer:1234"
	req1 := TransportEnvelope{MessageID: "timeout-1", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req1)
	if err == nil {
		t.Fatal("expected first request to time out")
	}
	if !IsTransportTimeout(err) {
		t.Fatalf("expected transport timeout error, got: %v", err)
	}
	if sm.HasSession(peerURL) {
		t.Fatal("session should be torn down after timeout")
	}

	start := time.Now()
	req2 := TransportEnvelope{MessageID: "timeout-2", MessageType: "test"}
	resp, err := sm.Request(peerURL, serverPub, req2)
	if err != nil {
		t.Fatalf("second request should reconnect and succeed: %v", err)
	}
	if resp.RetryOf != req2.MessageID {
		t.Fatalf("second response RetryOf=%q, want %q", resp.RetryOf, req2.MessageID)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected reconnect well before idle timeout, got %v", elapsed)
	}
	if got := connectionCount.Load(); got != 2 {
		t.Fatalf("expected 2 connections after timeout-triggered reconnect, got %d", got)
	}
}

func TestSessionReconnectAfterDrop(t *testing.T) {
	serverPriv, serverPub := testKeypair(t)

	var listenerMu sync.Mutex
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	defer listener.Close()

	// Track connections to close the first one.
	connections := make(chan net.Conn, 10)
	go func() {
		for {
			listenerMu.Lock()
			l := listener
			listenerMu.Unlock()
			conn, err := l.Accept()
			if err != nil {
				return
			}
			connections <- conn
			go func() {
				preface := make([]byte, V3PrefaceLen())
				io.ReadFull(conn, preface)
				noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
				if err != nil {
					conn.Close()
					return
				}
				echoHandler(conn, noise)
			}()
		}
	}()

	metrics := &SessionMetrics{}
	cfg := SessionConfig{
		ConnectTimeout:    2 * time.Second,
		HandshakeTimeout:  2 * time.Second,
		RequestTimeout:    3 * time.Second,
		IdleTimeout:       10 * time.Second,
		KeepaliveInterval: 60 * time.Second,
	}
	sm := NewSessionManager(cfg, metrics, func(string) (net.Conn, error) {
		return net.DialTimeout("tcp", addr, 2*time.Second)
	})
	defer sm.CloseAll()

	peerURL := "parker://reconnect-peer:1234"

	// First request succeeds.
	req1 := TransportEnvelope{MessageID: "r1", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	// Kill the server-side connection to simulate a drop.
	firstConn := <-connections
	firstConn.Close()

	// Give the read loop a moment to detect the broken connection.
	time.Sleep(100 * time.Millisecond)

	// Second request should reconnect and succeed.
	req2 := TransportEnvelope{MessageID: "r2", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req2)
	if err != nil {
		t.Fatalf("second request (after reconnect) failed: %v", err)
	}
}

func TestSessionRequestRejectsWireVersionMismatch(t *testing.T) {
	serverHandler := func(conn net.Conn, noise *NoiseSession) {
		defer conn.Close()
		req, err := ReadV3Request(conn, noise, 10*time.Second)
		if err != nil {
			return
		}
		resp := TransportEnvelope{
			MessageType:          req.MessageType + ".response",
			MessageID:            "resp-" + req.MessageID,
			RetryOf:              req.MessageID,
			TransportWireVersion: 2,
		}
		_ = WriteV3Response(conn, noise, resp, 5*time.Second)
	}

	sm, serverPub, _, listener := testSessionPair(t, serverHandler)
	defer listener.Close()
	defer sm.CloseAll()

	peerURL := "parker://wire-version-peer:1234"
	_, err := sm.Request(peerURL, serverPub, TransportEnvelope{
		MessageID:            "wire-version-1",
		MessageType:          "test",
		TransportWireVersion: 3,
	})
	if err == nil {
		t.Fatal("expected wire version mismatch error")
	}
	if !IsWireDowngrade(err) {
		t.Fatalf("expected wire version downgrade error, got %v", err)
	}
	if sm.HasSession(peerURL) {
		t.Fatal("session should be torn down after a wire version mismatch")
	}
}

func TestSessionCloseAll(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()

	peerURL := "parker://close-peer:1234"
	req := TransportEnvelope{MessageID: "c1", MessageType: "test"}
	_, err := sm.Request(peerURL, serverPub, req)
	if err != nil {
		t.Fatal(err)
	}

	sm.CloseAll()

	// Requests after close should fail.
	_, err = sm.Request(peerURL, serverPub, req)
	if err == nil {
		t.Fatal("expected error after CloseAll")
	}
}

func TestSessionCloseSpecificPeer(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()
	defer sm.CloseAll()

	peerURL := "parker://specific-peer:1234"
	req := TransportEnvelope{MessageID: "s1", MessageType: "test"}
	_, err := sm.Request(peerURL, serverPub, req)
	if err != nil {
		t.Fatal(err)
	}
	if !sm.HasSession(peerURL) {
		t.Fatal("session should exist")
	}

	sm.CloseSession(peerURL)
	if sm.HasSession(peerURL) {
		t.Fatal("session should not exist after CloseSession")
	}
}

func TestV3Preface(t *testing.T) {
	if !IsV3Preface([]byte("PARKERv3")) {
		t.Fatal("should match")
	}
	if IsV3Preface([]byte("PARKERv2")) {
		t.Fatal("should not match v2")
	}
	if IsV3Preface([]byte("{")) {
		t.Fatal("should not match JSON")
	}
	if IsV3Preface(nil) {
		t.Fatal("should not match nil")
	}
}

func TestFrameReadWrite(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	data := []byte(`{"messageId":"test-123","messageType":"test"}`)
	go func() {
		if err := writeFrameBytes(clientConn, data); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	got, err := readFrameBytes(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

func TestErrorTypes(t *testing.T) {
	timeout := &TransportError{Kind: ErrKindRequestTimeout, PeerURL: "test", Detail: "test timeout"}
	if !IsTransportTimeout(timeout) {
		t.Fatal("should be timeout")
	}
	if IsSessionReset(timeout) {
		t.Fatal("should not be session reset")
	}
	if IsWireDowngrade(timeout) {
		t.Fatal("should not be wire downgrade")
	}

	reset := &TransportError{Kind: ErrKindSessionReset}
	if !IsSessionReset(reset) {
		t.Fatal("should be session reset")
	}

	downgrade := &TransportError{Kind: ErrKindWireDowngrade}
	if !IsWireDowngrade(downgrade) {
		t.Fatal("should be wire downgrade")
	}

	// Test Error() output.
	msg := timeout.Error()
	if msg == "" {
		t.Fatal("error message should not be empty")
	}
}

func TestConfigDefaults(t *testing.T) {
	direct := DefaultSessionConfig(false)
	if direct.ConnectTimeout != 5*time.Second {
		t.Fatalf("direct connect timeout: got %v, want 5s", direct.ConnectTimeout)
	}
	if direct.RequestTimeout != 8*time.Second {
		t.Fatalf("direct request timeout: got %v, want 8s", direct.RequestTimeout)
	}

	tor := DefaultSessionConfig(true)
	if tor.ConnectTimeout != 12*time.Second {
		t.Fatalf("tor connect timeout: got %v, want 12s", tor.ConnectTimeout)
	}
	if tor.RequestTimeout != 20*time.Second {
		t.Fatalf("tor request timeout: got %v, want 20s", tor.RequestTimeout)
	}
}

func TestMetricsSnapshot(t *testing.T) {
	m := &SessionMetrics{}
	m.HandshakeCount.Add(5)
	m.HandshakeDurationNs.Add(500_000_000) // 500ms total
	m.SessionReuseCount.Add(10)
	m.ReconnectCount.Add(2)
	m.RequestTimeoutCount.Add(1)
	m.FallbackToV2Count.Add(3)

	snap := m.Snapshot()
	if snap.HandshakeCount != 5 {
		t.Fatalf("got %d, want 5", snap.HandshakeCount)
	}
	if snap.AvgHandshakeDurationNs() != 100_000_000 {
		t.Fatalf("avg handshake duration: got %d, want 100000000", snap.AvgHandshakeDurationNs())
	}
	if snap.SessionReuseCount != 10 {
		t.Fatalf("reuse: got %d, want 10", snap.SessionReuseCount)
	}
	if snap.FallbackToV2Count != 3 {
		t.Fatalf("fallback: got %d, want 3", snap.FallbackToV2Count)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
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
				preface := make([]byte, V3PrefaceLen())
				io.ReadFull(conn, preface)
				noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
				if err != nil {
					conn.Close()
					return
				}
				echoHandler(conn, noise)
			}()
		}
	}()

	metrics := &SessionMetrics{}
	cfg := SessionConfig{
		ConnectTimeout:    2 * time.Second,
		HandshakeTimeout:  2 * time.Second,
		RequestTimeout:    3 * time.Second,
		IdleTimeout:       300 * time.Millisecond, // very short
		KeepaliveInterval: 60 * time.Second,
	}
	sm := NewSessionManager(cfg, metrics, func(string) (net.Conn, error) {
		return net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	})
	defer sm.CloseAll()

	peerURL := "parker://idle-peer:1234"

	// First request succeeds.
	req := TransportEnvelope{MessageID: "idle-1", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for idle timeout to trigger read loop close.
	time.Sleep(500 * time.Millisecond)

	// Session should be detected as closed, triggering reconnect.
	req2 := TransportEnvelope{MessageID: "idle-2", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req2)
	if err != nil {
		t.Fatalf("request after idle timeout should reconnect, got: %v", err)
	}
}

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	env := TransportEnvelope{
		MessageID:            "test-123",
		MessageType:          "test.request",
		Channel:              "table",
		SenderPeerID:         "peer-abc",
		TransportWireVersion: 3,
		BodyCiphertext:       "dGVzdA",
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TransportEnvelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.MessageID != env.MessageID {
		t.Fatalf("MessageID mismatch: %q vs %q", decoded.MessageID, env.MessageID)
	}
	if decoded.TransportWireVersion != 3 {
		t.Fatalf("wire version: got %d, want 3", decoded.TransportWireVersion)
	}
}

func TestSessionConcurrentRequestAndClose(t *testing.T) {
	sm, serverPub, _, listener := testSessionPair(t, echoHandler)
	defer listener.Close()

	peerURL := "parker://race-peer:1234"

	// Establish a session first.
	req := TransportEnvelope{MessageID: "race-setup", MessageType: "test"}
	_, err := sm.Request(peerURL, serverPub, req)
	if err != nil {
		t.Fatal(err)
	}

	// Fire concurrent requests while closing.
	const numRequests = 20
	var wg sync.WaitGroup
	wg.Add(numRequests + 1)

	for i := 0; i < numRequests; i++ {
		go func(idx int) {
			defer wg.Done()
			r := TransportEnvelope{
				MessageID:   fmt.Sprintf("race-%d", idx),
				MessageType: "test",
			}
			// Either succeeds or returns a session error — must not panic.
			sm.Request(peerURL, serverPub, r)
		}(i)
	}
	go func() {
		defer wg.Done()
		sm.CloseAll()
	}()

	wg.Wait()
}

func TestKeepalivePreventsIdleTimeout(t *testing.T) {
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
				preface := make([]byte, V3PrefaceLen())
				io.ReadFull(conn, preface)
				noise, err := AcceptV3Session(conn, serverPriv, serverPub, 5*time.Second)
				if err != nil {
					conn.Close()
					return
				}
				echoHandler(conn, noise)
			}()
		}
	}()

	metrics := &SessionMetrics{}
	cfg := SessionConfig{
		ConnectTimeout:    2 * time.Second,
		HandshakeTimeout:  2 * time.Second,
		RequestTimeout:    3 * time.Second,
		IdleTimeout:       400 * time.Millisecond,
		KeepaliveInterval: 150 * time.Millisecond, // fires well within idle timeout
	}
	sm := NewSessionManager(cfg, metrics, func(string) (net.Conn, error) {
		return net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	})
	defer sm.CloseAll()

	peerURL := "parker://keepalive-peer:1234"

	// First request establishes the session.
	req := TransportEnvelope{MessageID: "ka-1", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req)
	if err != nil {
		t.Fatal(err)
	}

	// Wait longer than idle timeout; keepalives should keep the session alive.
	time.Sleep(700 * time.Millisecond)

	if !sm.HasSession(peerURL) {
		t.Fatal("session should still be alive due to keepalives")
	}

	// Another request should succeed on the same session (no reconnect needed).
	req2 := TransportEnvelope{MessageID: "ka-2", MessageType: "test"}
	_, err = sm.Request(peerURL, serverPub, req2)
	if err != nil {
		t.Fatalf("request after keepalive period should succeed: %v", err)
	}
}

func TestErrorsAsWrapped(t *testing.T) {
	inner := &TransportError{Kind: ErrKindRequestTimeout, PeerURL: "test"}
	wrapped := fmt.Errorf("outer: %w", inner)

	if !IsTransportTimeout(wrapped) {
		t.Fatal("IsTransportTimeout should work through wrapping")
	}
	if IsSessionReset(wrapped) {
		t.Fatal("IsSessionReset should not match timeout")
	}

	resetInner := &TransportError{Kind: ErrKindSessionReset}
	resetWrapped := fmt.Errorf("outer: %w", resetInner)
	if !IsSessionReset(resetWrapped) {
		t.Fatal("IsSessionReset should work through wrapping")
	}

	downgradeInner := &TransportError{Kind: ErrKindWireDowngrade}
	downgradeWrapped := fmt.Errorf("outer: %w", downgradeInner)
	if !IsWireDowngrade(downgradeWrapped) {
		t.Fatal("IsWireDowngrade should work through wrapping")
	}
}
