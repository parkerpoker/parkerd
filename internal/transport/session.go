package transport

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// V3 session preface sent by the initiator before the Noise handshake.
// This lets the responder distinguish v3 session connections from v2 one-shot connections.
var v3SessionPreface = []byte("PARKERv3")

// SessionManager maintains persistent outbound sessions keyed by peer URL.
type SessionManager struct {
	mu         sync.Mutex
	sessions   map[string]*OutboundSession
	inflight   map[string]chan struct{} // closed when a dial-in-progress completes
	peerGen    map[string]uint64       // per-peer generation; bumped on invalidation
	config     SessionConfig
	metrics    *SessionMetrics
	dialer     func(peerURL string) (net.Conn, error)
	closed     bool
}

// NewSessionManager creates a session manager with the given config and dial function.
func NewSessionManager(config SessionConfig, metrics *SessionMetrics, dialer func(string) (net.Conn, error)) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*OutboundSession),
		inflight: make(map[string]chan struct{}),
		peerGen:  make(map[string]uint64),
		config:   config,
		metrics:  metrics,
		dialer:   dialer,
	}
}

// Request sends a request envelope over a persistent session to peerURL and returns
// the response. It reuses an existing session or creates a new one if needed.
// peerStaticPub is the 32-byte X25519 public key of the peer (from discovery).
func (sm *SessionManager) Request(peerURL string, peerStaticPub []byte, request TransportEnvelope) (TransportEnvelope, error) {
	sess, err := sm.getOrCreateSession(peerURL, peerStaticPub)
	if err != nil {
		return TransportEnvelope{}, err
	}

	sm.metrics.SessionReuseCount.Add(1)

	resp, err := sess.RoundTrip(request, sm.config.RequestTimeout)
	if err != nil {
		// Tear down on broken connections AND timeouts. The server handles
		// one request at a time per v3 session, so a timed-out request
		// leaves the connection in an unknown state — reusing it would
		// cause subsequent requests to time out until idle expiry.
		if isConnBroken(err) || IsTransportTimeout(err) {
			sm.removeSession(peerURL)
		}
		return TransportEnvelope{}, err
	}
	if request.TransportWireVersion != 0 && resp.TransportWireVersion != request.TransportWireVersion {
		sm.removeSession(peerURL)
		return TransportEnvelope{}, &TransportError{
			Kind:    ErrKindWireDowngrade,
			PeerURL: peerURL,
			Detail:  fmt.Sprintf("expected wire version %d, got %d", request.TransportWireVersion, resp.TransportWireVersion),
		}
	}
	return resp, nil
}

// CloseAll tears down all outbound sessions.
func (sm *SessionManager) CloseAll() {
	sm.mu.Lock()
	sm.closed = true
	sessions := make([]*OutboundSession, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		sessions = append(sessions, s)
	}
	sm.sessions = make(map[string]*OutboundSession)
	sm.mu.Unlock()

	for _, s := range sessions {
		s.CloseAndWait()
	}
}

// CloseSession tears down the session for a specific peer URL (e.g. on key rotation).
// It also bumps the peer generation so that any in-flight dial with the old key
// will be discarded when it completes, preventing stale sessions from being installed.
func (sm *SessionManager) CloseSession(peerURL string) {
	sm.mu.Lock()
	sess, ok := sm.sessions[peerURL]
	if ok {
		delete(sm.sessions, peerURL)
	}
	sm.peerGen[peerURL]++
	sm.mu.Unlock()
	if ok {
		sess.CloseAndWait()
	}
}

// HasSession returns true if there is an active session for the given peer URL.
func (sm *SessionManager) HasSession(peerURL string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sess, ok := sm.sessions[peerURL]
	return ok && !sess.isClosed()
}

func (sm *SessionManager) getOrCreateSession(peerURL string, peerStaticPub []byte) (*OutboundSession, error) {
	for {
		sm.mu.Lock()
		if sm.closed {
			sm.mu.Unlock()
			return nil, &TransportError{Kind: ErrKindSessionClosed, PeerURL: peerURL, Detail: "session manager closed"}
		}
		sess, ok := sm.sessions[peerURL]
		if ok && !sess.isClosed() {
			sm.mu.Unlock()
			return sess, nil
		}
		// Another goroutine is already dialing this peer — wait for it.
		if wait, exists := sm.inflight[peerURL]; exists {
			sm.mu.Unlock()
			<-wait
			continue
		}
		// We are the dialer for this peer.
		done := make(chan struct{})
		sm.inflight[peerURL] = done
		wasReconnect := ok // old entry existed but was closed
		genAtDial := sm.peerGen[peerURL]
		sm.mu.Unlock()

		newSess, err := sm.dialAndHandshake(peerURL, peerStaticPub)

		sm.mu.Lock()
		delete(sm.inflight, peerURL)
		close(done)

		if err != nil {
			sm.mu.Unlock()
			return nil, err
		}
		if sm.closed {
			sm.mu.Unlock()
			newSess.Close()
			return nil, &TransportError{Kind: ErrKindSessionClosed, PeerURL: peerURL, Detail: "session manager closed"}
		}
		// If the peer was invalidated (e.g. key rotation) while we were dialing,
		// discard this session — it was authenticated against the old key.
		if sm.peerGen[peerURL] != genAtDial {
			sm.mu.Unlock()
			newSess.Close()
			return nil, &TransportError{Kind: ErrKindSessionReset, PeerURL: peerURL, Detail: "peer key rotated during dial"}
		}
		// Double-check: another path may have inserted a session.
		if existing, ok := sm.sessions[peerURL]; ok && !existing.isClosed() {
			sm.mu.Unlock()
			newSess.Close()
			return existing, nil
		}
		sm.sessions[peerURL] = newSess
		sm.mu.Unlock()
		if wasReconnect {
			sm.metrics.ReconnectCount.Add(1)
		}
		return newSess, nil
	}
}

func (sm *SessionManager) dialAndHandshake(peerURL string, peerStaticPub []byte) (*OutboundSession, error) {
	conn, err := sm.dialWithTimeout(peerURL)
	if err != nil {
		return nil, err
	}

	// Set a single deadline covering preface write + Noise handshake.
	deadline := time.Now().Add(sm.config.HandshakeTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, &TransportError{Kind: ErrKindHandshakeFailed, PeerURL: peerURL, Detail: "set deadline", Cause: err}
	}

	// Write session preface.
	if _, err := conn.Write(v3SessionPreface); err != nil {
		conn.Close()
		return nil, &TransportError{Kind: ErrKindHandshakeFailed, PeerURL: peerURL, Detail: "preface write", Cause: err}
	}

	// Perform Noise NK handshake.
	hsStart := time.Now()
	noise, err := NoiseNKInitiator(conn, peerStaticPub)
	if err != nil {
		conn.Close()
		return nil, &TransportError{Kind: ErrKindHandshakeFailed, PeerURL: peerURL, Detail: "noise handshake", Cause: err}
	}
	hsDur := time.Since(hsStart)
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, &TransportError{Kind: ErrKindHandshakeFailed, PeerURL: peerURL, Detail: "clear deadline", Cause: err}
	}

	sm.metrics.HandshakeCount.Add(1)
	sm.metrics.HandshakeDurationNs.Add(hsDur.Nanoseconds())

	sess := newOutboundSession(conn, noise, peerURL, sm.config)
	return sess, nil
}

func (sm *SessionManager) dialWithTimeout(peerURL string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, e := sm.dialer(peerURL)
		ch <- result{c, e}
	}()
	timer := time.NewTimer(sm.config.ConnectTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, &TransportError{Kind: ErrKindHandshakeFailed, PeerURL: peerURL, Detail: "dial failed", Cause: r.err}
		}
		return r.conn, nil
	case <-timer.C:
		// Clean up the connection if the dial eventually succeeds.
		go func() {
			r := <-ch
			if r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, &TransportError{Kind: ErrKindHandshakeFailed, PeerURL: peerURL, Detail: "connect timeout"}
	}
}

func (sm *SessionManager) removeSession(peerURL string) {
	sm.mu.Lock()
	sess, ok := sm.sessions[peerURL]
	if ok {
		delete(sm.sessions, peerURL)
	}
	sm.mu.Unlock()
	if ok {
		sess.CloseAndWait()
	}
}

// maxDecryptedFrameSize limits the size of decrypted plaintext to prevent DoS
// from a malicious peer sending huge encrypted payloads.
const maxDecryptedFrameSize = 4 * 1024 * 1024 // 4 MiB

// OutboundSession wraps a single persistent TCP + Noise session to a peer.
type OutboundSession struct {
	conn    net.Conn
	noise   *NoiseSession
	peerURL string
	config  SessionConfig

	writeMu      sync.Mutex
	pending      sync.Map // messageID -> chan TransportEnvelope
	closeOnce    sync.Once
	closeCh      chan struct{}
	closeErr     atomic.Value // stores error
	wg           sync.WaitGroup
	keepaliveSeq atomic.Uint64
}

func newOutboundSession(conn net.Conn, noise *NoiseSession, peerURL string, config SessionConfig) *OutboundSession {
	s := &OutboundSession{
		conn:    conn,
		noise:   noise,
		peerURL: peerURL,
		config:  config,
		closeCh: make(chan struct{}),
	}
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.readLoop()
	}()
	go func() {
		defer s.wg.Done()
		s.keepaliveLoop()
	}()
	return s
}

func (s *OutboundSession) isClosed() bool {
	select {
	case <-s.closeCh:
		return true
	default:
		return false
	}
}

func (s *OutboundSession) Close() {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		s.conn.Close()
		// Drain all pending requests.
		s.pending.Range(func(key, value any) bool {
			if ch, ok := value.(chan TransportEnvelope); ok {
				select {
				case ch <- TransportEnvelope{}:
				default:
				}
			}
			s.pending.Delete(key)
			return true
		})
	})
}

// CloseAndWait closes the session and waits for background goroutines to exit.
// External callers (e.g. SessionManager) use this for clean shutdown.
func (s *OutboundSession) CloseAndWait() {
	s.Close()
	s.wg.Wait()
}

// RoundTrip sends a request and waits for the correlated response.
func (s *OutboundSession) RoundTrip(request TransportEnvelope, timeout time.Duration) (TransportEnvelope, error) {
	respCh := make(chan TransportEnvelope, 1)
	s.pending.Store(request.MessageID, respCh)
	defer s.pending.Delete(request.MessageID)

	// Check closed after storing so that Close() will drain this channel.
	if s.isClosed() {
		return TransportEnvelope{}, &TransportError{Kind: ErrKindSessionClosed, PeerURL: s.peerURL}
	}

	if err := s.writeFrame(request); err != nil {
		return TransportEnvelope{}, &TransportError{Kind: ErrKindSessionReset, PeerURL: s.peerURL, Detail: "write request", Cause: err}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-respCh:
		if resp.MessageID == "" && resp.MessageType == "" {
			return TransportEnvelope{}, &TransportError{Kind: ErrKindSessionReset, PeerURL: s.peerURL, Detail: "session closed while waiting"}
		}
		return resp, nil
	case <-timer.C:
		return TransportEnvelope{}, &TransportError{Kind: ErrKindRequestTimeout, PeerURL: s.peerURL, Detail: fmt.Sprintf("timeout after %s", timeout)}
	case <-s.closeCh:
		return TransportEnvelope{}, &TransportError{Kind: ErrKindSessionClosed, PeerURL: s.peerURL}
	}
}

func (s *OutboundSession) writeFrame(envelope TransportEnvelope) error {
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	// Encrypt and write must be serialized together: nonce allocation in
	// Encrypt must match wire order, otherwise the receiver will see
	// out-of-order nonces and AEAD decryption will fail.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	encrypted, err := s.noise.Encrypt(data)
	if err != nil {
		return err
	}

	_ = s.conn.SetWriteDeadline(time.Now().Add(s.config.RequestTimeout))
	return writeFrameBytes(s.conn, encrypted)
}

func (s *OutboundSession) readLoop() {
	defer s.Close()
	for {
		select {
		case <-s.closeCh:
			return
		default:
		}

		_ = s.conn.SetReadDeadline(time.Now().Add(s.config.IdleTimeout))
		frame, err := readFrameBytes(s.conn)
		if err != nil {
			s.closeErr.CompareAndSwap(nil, err)
			return
		}

		plaintext, err := s.noise.Decrypt(frame)
		if err != nil {
			s.closeErr.CompareAndSwap(nil, err)
			return
		}
		if len(plaintext) > maxDecryptedFrameSize {
			s.closeErr.CompareAndSwap(nil, errors.New("decrypted frame exceeds max size"))
			return
		}

		var resp TransportEnvelope
		if err := json.Unmarshal(plaintext, &resp); err != nil {
			s.closeErr.CompareAndSwap(nil, err)
			return
		}

		// Route response to pending request by RetryOf (which holds the request MessageID).
		requestID := resp.RetryOf
		if requestID == "" {
			requestID = resp.MessageID
		}
		if ch, ok := s.pending.LoadAndDelete(requestID); ok {
			if respCh, ok := ch.(chan TransportEnvelope); ok {
				select {
				case respCh <- resp:
				default:
				}
			}
		}
	}
}

func (s *OutboundSession) keepaliveLoop() {
	defer s.Close()
	ticker := time.NewTicker(s.config.KeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-ticker.C:
			seq := s.keepaliveSeq.Add(1)
			ping := TransportEnvelope{
				MessageType: "keepalive",
				MessageID:   "keepalive-" + strconv.FormatUint(seq, 10),
			}
			if err := s.writeFrame(ping); err != nil {
				s.closeErr.CompareAndSwap(nil, err)
				return
			}
		}
	}
}

// Frame format for encrypted session messages: 4-byte big-endian length + ciphertext.
func writeFrameBytes(w io.Writer, data []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func readFrameBytes(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > 16*1024*1024 {
		return nil, errors.New("invalid frame size")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func isConnBroken(err error) bool {
	if err == nil {
		return false
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	var te *TransportError
	if errors.As(err, &te) {
		return te.Kind == ErrKindSessionReset || te.Kind == ErrKindSessionClosed || te.Kind == ErrKindHandshakeFailed
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// AcceptV3Session performs the server-side v3 handshake on an already-accepted connection
// where the v3 preface has already been read. Returns a NoiseSession for encrypted I/O.
func AcceptV3Session(conn net.Conn, staticPriv, staticPub []byte, timeout time.Duration) (*NoiseSession, error) {
	_ = conn.SetDeadline(time.Now().Add(timeout))
	noise, err := NoiseNKResponder(conn, staticPriv, staticPub)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return noise, nil
}

// ReadV3Request reads and decrypts one request frame from an established v3 session.
func ReadV3Request(conn net.Conn, noise *NoiseSession, readTimeout time.Duration) (TransportEnvelope, error) {
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	frame, err := readFrameBytes(conn)
	if err != nil {
		return TransportEnvelope{}, err
	}
	plaintext, err := noise.Decrypt(frame)
	if err != nil {
		return TransportEnvelope{}, err
	}
	var envelope TransportEnvelope
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		return TransportEnvelope{}, err
	}
	return envelope, nil
}

// WriteV3Response encrypts and writes a response frame on an established v3 session.
func WriteV3Response(conn net.Conn, noise *NoiseSession, response TransportEnvelope, writeTimeout time.Duration) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	encrypted, err := noise.Encrypt(data)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return writeFrameBytes(conn, encrypted)
}

// IsV3Preface checks if the given bytes match the v3 session preface.
func IsV3Preface(data []byte) bool {
	if len(data) < len(v3SessionPreface) {
		return false
	}
	for i, b := range v3SessionPreface {
		if data[i] != b {
			return false
		}
	}
	return true
}

// V3PrefaceLen returns the length of the v3 session preface.
func V3PrefaceLen() int {
	return len(v3SessionPreface)
}
