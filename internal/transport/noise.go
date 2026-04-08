package transport

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

// Noise NK handshake implementation using X25519 + ChaChaPoly.
//
// Pattern NK:
//   <- s                         (responder static key known to initiator via discovery)
//   ...
//   -> e, es                     (initiator sends ephemeral, mixes DH(e, s))
//   <- e, ee                     (responder sends ephemeral, mixes DH(e_r, e_i))
//
// After handshake both sides derive two CipherState objects for
// initiator→responder and responder→initiator directions.

const (
	noiseProtocolName = "Noise_NK_25519_ChaChaPoly_SHA256"
	dhKeyLen          = 32
	tagLen            = chacha20poly1305.Overhead // 16
)

// noiseSymmetricState holds the evolving handshake hash, chaining key, and
// optional cipher key for within-handshake encryption (per Noise spec §5.2).
type noiseSymmetricState struct {
	h    [32]byte // handshake hash
	ck   [32]byte // chaining key
	k    [32]byte // cipher key set by mixKey
	n    uint64   // nonce counter for handshake encryption
	hasK bool     // true after first mixKey call
}

func newSymmetricState() noiseSymmetricState {
	// Per Noise spec section 5.2: if the protocol name is ≤ 32 bytes,
	// h and ck are initialized by zero-padding the name to 32 bytes.
	// Only if the name exceeds 32 bytes do we hash it.
	name := []byte(noiseProtocolName)
	var h [32]byte
	if len(name) <= 32 {
		copy(h[:], name)
	} else {
		h = sha256.Sum256(name)
	}
	return noiseSymmetricState{h: h, ck: h}
}

func (s *noiseSymmetricState) mixHash(data []byte) {
	h := sha256.New()
	h.Write(s.h[:])
	h.Write(data)
	copy(s.h[:], h.Sum(nil))
}

// hkdfSHA256 derives two 32-byte keys from chaining key and input key material.
func hkdfSHA256(ck, ikm []byte) ([32]byte, [32]byte) {
	// HKDF-Extract
	tempKey := hmacSHA256(ck, ikm)
	// HKDF-Expand (output1 = HMAC(tempKey, 0x01))
	out1 := hmacSHA256(tempKey[:], []byte{0x01})
	// HKDF-Expand (output2 = HMAC(tempKey, out1 || 0x02))
	buf := make([]byte, 33)
	copy(buf, out1[:])
	buf[32] = 0x02
	out2 := hmacSHA256(tempKey[:], buf)
	return out1, out2
}

func hmacSHA256(key, data []byte) [32]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func (s *noiseSymmetricState) mixKey(dhOutput []byte) {
	s.ck, s.k = hkdfSHA256(s.ck[:], dhOutput)
	s.n = 0
	s.hasK = true
}

// encryptAndHash encrypts plaintext using the handshake cipher key (if set)
// with the handshake hash as associated data, then mixes the ciphertext into h.
func (s *noiseSymmetricState) encryptAndHash(plaintext []byte) ([]byte, error) {
	if !s.hasK {
		s.mixHash(plaintext)
		return append([]byte(nil), plaintext...), nil
	}
	aead, err := chacha20poly1305.New(s.k[:])
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], s.n)
	s.n++
	ciphertext := aead.Seal(nil, nonce[:], plaintext, s.h[:])
	s.mixHash(ciphertext)
	return ciphertext, nil
}

// decryptAndHash decrypts ciphertext using the handshake cipher key (if set)
// with the handshake hash as associated data, then mixes the ciphertext into h.
func (s *noiseSymmetricState) decryptAndHash(ciphertext []byte) ([]byte, error) {
	if !s.hasK {
		s.mixHash(ciphertext)
		return append([]byte(nil), ciphertext...), nil
	}
	aead, err := chacha20poly1305.New(s.k[:])
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], s.n)
	s.n++
	plaintext, err := aead.Open(nil, nonce[:], ciphertext, s.h[:])
	if err != nil {
		return nil, err
	}
	s.mixHash(ciphertext)
	return plaintext, nil
}

// split derives the final two transport cipher states from the current chaining key.
func (s *noiseSymmetricState) split() (*noiseCipherState, *noiseCipherState) {
	k1, k2 := hkdfSHA256(s.ck[:], nil)
	return &noiseCipherState{key: k1}, &noiseCipherState{key: k2}
}

// noiseCipherState holds a symmetric key and nonce counter for one direction.
type noiseCipherState struct {
	key   [32]byte
	nonce uint64
	mu    sync.Mutex
}

func (cs *noiseCipherState) encrypt(plaintext, ad []byte) ([]byte, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	aead, err := chacha20poly1305.New(cs.key[:])
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], cs.nonce)
	cs.nonce++
	return aead.Seal(nil, nonce[:], plaintext, ad), nil
}

func (cs *noiseCipherState) decrypt(ciphertext, ad []byte) ([]byte, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	aead, err := chacha20poly1305.New(cs.key[:])
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], cs.nonce)
	cs.nonce++
	return aead.Open(nil, nonce[:], ciphertext, ad)
}

// x25519DH performs an ECDH key exchange.
func x25519DH(privBytes, pubBytes []byte) ([]byte, error) {
	priv, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		return nil, err
	}
	pub, err := ecdh.X25519().NewPublicKey(pubBytes)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

func generateX25519Keypair() (priv, pub []byte, err error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return key.Bytes(), key.PublicKey().Bytes(), nil
}

// NoiseSession holds the transport cipher states after a completed handshake.
type NoiseSession struct {
	Send *noiseCipherState
	Recv *noiseCipherState
}

// Encrypt encrypts a plaintext frame for sending.
func (ns *NoiseSession) Encrypt(plaintext []byte) ([]byte, error) {
	return ns.Send.encrypt(plaintext, nil)
}

// Decrypt decrypts a received ciphertext frame.
func (ns *NoiseSession) Decrypt(ciphertext []byte) ([]byte, error) {
	return ns.Recv.decrypt(ciphertext, nil)
}

// NoiseNKInitiator performs the client side of a Noise NK handshake.
// It writes/reads handshake messages on rw and returns the session on success.
// responderStaticPub is the 32-byte X25519 public key of the responder.
func NoiseNKInitiator(rw io.ReadWriter, responderStaticPub []byte) (*NoiseSession, error) {
	if len(responderStaticPub) != dhKeyLen {
		return nil, fmt.Errorf("responder static key must be %d bytes, got %d", dhKeyLen, len(responderStaticPub))
	}

	ss := newSymmetricState()
	// Pre-message: mix responder's static key into handshake hash
	ss.mixHash(responderStaticPub)

	// -> e, es
	ePriv, ePub, err := generateX25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("generate initiator ephemeral: %w", err)
	}
	ss.mixHash(ePub)

	dhES, err := x25519DH(ePriv, responderStaticPub)
	if err != nil {
		return nil, fmt.Errorf("DH(e, s): %w", err)
	}
	ss.mixKey(dhES)

	// Encrypt empty payload — the tag provides key confirmation for es.
	tag1, err := ss.encryptAndHash(nil)
	if err != nil {
		return nil, fmt.Errorf("encrypt handshake msg1 payload: %w", err)
	}

	// Send message 1: initiator ephemeral public key + encrypted payload (tag).
	msg1Out := make([]byte, 0, len(ePub)+len(tag1))
	msg1Out = append(msg1Out, ePub...)
	msg1Out = append(msg1Out, tag1...)
	if err := writeNoiseMsg(rw, msg1Out); err != nil {
		return nil, fmt.Errorf("write handshake msg1: %w", err)
	}

	// <- e, ee
	msg2, err := readNoiseMsg(rw)
	if err != nil {
		return nil, fmt.Errorf("read handshake msg2: %w", err)
	}
	if len(msg2) < dhKeyLen+tagLen {
		return nil, errors.New("invalid handshake msg2 length")
	}
	responderEPub := msg2[:dhKeyLen]
	ss.mixHash(responderEPub)

	dhEE, err := x25519DH(ePriv, responderEPub)
	if err != nil {
		return nil, fmt.Errorf("DH(e_i, e_r): %w", err)
	}
	ss.mixKey(dhEE)

	// Decrypt and verify responder's payload tag — key confirmation for ee.
	if _, err := ss.decryptAndHash(msg2[dhKeyLen:]); err != nil {
		return nil, fmt.Errorf("handshake msg2 key confirmation failed: %w", err)
	}

	// Split into transport keys
	c1, c2 := ss.split()
	return &NoiseSession{Send: c1, Recv: c2}, nil
}

// NoiseNKResponder performs the server side of a Noise NK handshake.
// staticPriv is the 32-byte X25519 private key; staticPub is the corresponding public key.
func NoiseNKResponder(rw io.ReadWriter, staticPriv, staticPub []byte) (*NoiseSession, error) {
	if len(staticPriv) != dhKeyLen || len(staticPub) != dhKeyLen {
		return nil, errors.New("static keypair must be 32 bytes each")
	}

	ss := newSymmetricState()
	// Pre-message: mix our static key into handshake hash
	ss.mixHash(staticPub)

	// -> e, es (read initiator ephemeral + encrypted payload)
	msg1, err := readNoiseMsg(rw)
	if err != nil {
		return nil, fmt.Errorf("read handshake msg1: %w", err)
	}
	if len(msg1) < dhKeyLen+tagLen {
		return nil, errors.New("invalid handshake msg1 length")
	}
	initiatorEPub := msg1[:dhKeyLen]
	ss.mixHash(initiatorEPub)

	dhES, err := x25519DH(staticPriv, initiatorEPub)
	if err != nil {
		return nil, fmt.Errorf("DH(s, e): %w", err)
	}
	ss.mixKey(dhES)

	// Decrypt and verify initiator's payload tag — key confirmation for es.
	if _, err := ss.decryptAndHash(msg1[dhKeyLen:]); err != nil {
		return nil, fmt.Errorf("handshake msg1 key confirmation failed: %w", err)
	}

	// <- e, ee
	ePriv, ePub, err := generateX25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("generate responder ephemeral: %w", err)
	}
	ss.mixHash(ePub)

	dhEE, err := x25519DH(ePriv, initiatorEPub)
	if err != nil {
		return nil, fmt.Errorf("DH(e_r, e_i): %w", err)
	}
	ss.mixKey(dhEE)

	// Encrypt empty payload — the tag provides key confirmation for ee.
	tag2, err := ss.encryptAndHash(nil)
	if err != nil {
		return nil, fmt.Errorf("encrypt handshake msg2 payload: %w", err)
	}

	// Send message 2: responder ephemeral public key + encrypted payload (tag).
	msg2Out := make([]byte, 0, len(ePub)+len(tag2))
	msg2Out = append(msg2Out, ePub...)
	msg2Out = append(msg2Out, tag2...)
	if err := writeNoiseMsg(rw, msg2Out); err != nil {
		return nil, fmt.Errorf("write handshake msg2: %w", err)
	}

	// Split into transport keys (reversed direction for responder)
	c1, c2 := ss.split()
	return &NoiseSession{Send: c2, Recv: c1}, nil
}

// Wire format for handshake messages: 2-byte big-endian length prefix + payload.
func writeNoiseMsg(w io.Writer, data []byte) error {
	if len(data) > 65535 {
		return errors.New("noise message too large")
	}
	header := [2]byte{}
	binary.BigEndian.PutUint16(header[:], uint16(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func readNoiseMsg(r io.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint16(header[:])
	if size == 0 {
		return nil, errors.New("empty noise message")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
