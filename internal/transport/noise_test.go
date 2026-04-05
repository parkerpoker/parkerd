package transport

import (
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
)

func generateTestKeypair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key.Bytes(), key.PublicKey().Bytes()
}

func TestNoiseNKHandshakeSuccess(t *testing.T) {
	serverPriv, serverPub := generateTestKeypair(t)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var clientSession, serverSession *NoiseSession
	var clientErr, serverErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		clientSession, clientErr = NoiseNKInitiator(clientConn, serverPub)
	}()
	go func() {
		defer wg.Done()
		serverSession, serverErr = NoiseNKResponder(serverConn, serverPriv, serverPub)
	}()
	wg.Wait()

	if clientErr != nil {
		t.Fatalf("initiator error: %v", clientErr)
	}
	if serverErr != nil {
		t.Fatalf("responder error: %v", serverErr)
	}

	// Verify bidirectional encrypted communication.
	plaintext := []byte("hello from initiator")
	ct, err := clientSession.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := serverSession.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != string(plaintext) {
		t.Fatalf("decrypted mismatch: got %q, want %q", pt, plaintext)
	}

	// Reverse direction.
	plaintext2 := []byte("hello from responder")
	ct2, err := serverSession.Encrypt(plaintext2)
	if err != nil {
		t.Fatal(err)
	}
	pt2, err := clientSession.Decrypt(ct2)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt2) != string(plaintext2) {
		t.Fatalf("reverse decrypted mismatch: got %q, want %q", pt2, plaintext2)
	}
}

func TestNoiseNKWrongStaticKey(t *testing.T) {
	_, serverPub := generateTestKeypair(t)
	wrongPriv, wrongPub := generateTestKeypair(t)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var clientSession, serverSession *NoiseSession
	var clientErr, serverErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// Client uses the correct server public key.
		clientSession, clientErr = NoiseNKInitiator(clientConn, serverPub)
	}()
	go func() {
		defer wg.Done()
		// Server uses a WRONG keypair.
		serverSession, serverErr = NoiseNKResponder(serverConn, wrongPriv, wrongPub)
	}()
	wg.Wait()

	// The handshake itself may succeed (Noise NK doesn't authenticate the responder's
	// ephemeral in msg2 against the static key), but the derived keys will differ,
	// so encryption/decryption will fail.
	if clientErr != nil || serverErr != nil {
		// If handshake itself fails, that's also acceptable.
		return
	}

	plaintext := []byte("test message")
	ct, err := clientSession.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	_, err = serverSession.Decrypt(ct)
	if err == nil {
		t.Fatal("expected decryption to fail with wrong static key, but it succeeded")
	}
}

func TestNoiseNKMultipleMessages(t *testing.T) {
	serverPriv, serverPub := generateTestKeypair(t)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var clientSession, serverSession *NoiseSession
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		clientSession, _ = NoiseNKInitiator(clientConn, serverPub)
	}()
	go func() {
		defer wg.Done()
		serverSession, _ = NoiseNKResponder(serverConn, serverPriv, serverPub)
	}()
	wg.Wait()

	// Send multiple messages to verify nonce counter increments correctly.
	for i := 0; i < 100; i++ {
		msg := []byte("message number")
		ct, err := clientSession.Encrypt(msg)
		if err != nil {
			t.Fatalf("encrypt %d: %v", i, err)
		}
		pt, err := serverSession.Decrypt(ct)
		if err != nil {
			t.Fatalf("decrypt %d: %v", i, err)
		}
		if string(pt) != string(msg) {
			t.Fatalf("mismatch at %d", i)
		}
	}
}

func TestNoiseNKInitiatorBadKeyLength(t *testing.T) {
	clientConn, _ := net.Pipe()
	defer clientConn.Close()

	_, err := NoiseNKInitiator(clientConn, []byte("short"))
	if err == nil {
		t.Fatal("expected error for bad key length")
	}
}

func TestNoiseNKResponderBadKeyLength(t *testing.T) {
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	_, err := NoiseNKResponder(serverConn, []byte("short"), []byte("short"))
	if err == nil {
		t.Fatal("expected error for bad key length")
	}
}

func TestNoiseNKConnectionClosedDuringHandshake(t *testing.T) {
	_, serverPub := generateTestKeypair(t)

	clientConn, serverConn := net.Pipe()
	// Close server side immediately.
	serverConn.Close()

	_, err := NoiseNKInitiator(clientConn, serverPub)
	if err == nil {
		t.Fatal("expected error when server connection closed")
	}
	clientConn.Close()
}

func TestNoiseNKSymmetricStateInit(t *testing.T) {
	// The protocol name "Noise_NK_25519_ChaChaPoly_SHA256" is exactly 32 bytes.
	// Per Noise spec section 5.2, h and ck must be initialized by zero-padding
	// the name, NOT by hashing it.
	name := []byte("Noise_NK_25519_ChaChaPoly_SHA256")
	if len(name) != 32 {
		t.Fatalf("protocol name length: got %d, want 32", len(name))
	}

	ss := newSymmetricState()

	// Verify h equals zero-padded name (which is just the name since len == 32).
	var expected [32]byte
	copy(expected[:], name)
	if ss.h != expected {
		t.Fatalf("h should be zero-padded protocol name, not SHA256 hash.\ngot:  %x\nwant: %x", ss.h, expected)
	}
	if ss.ck != expected {
		t.Fatalf("ck should be zero-padded protocol name, not SHA256 hash.\ngot:  %x\nwant: %x", ss.ck, expected)
	}
}

func TestNoiseFraming(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	data := []byte("test framing data")
	go func() {
		if err := writeNoiseMsg(w, data); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	got, err := readNoiseMsg(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}
