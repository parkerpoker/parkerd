package transport

import "sync/atomic"

// SessionMetrics tracks observable counters for the v3 session layer.
// All fields are safe for concurrent access.
type SessionMetrics struct {
	HandshakeCount     atomic.Int64
	HandshakeDurationNs atomic.Int64 // cumulative nanoseconds across all handshakes
	SessionReuseCount  atomic.Int64
	ReconnectCount     atomic.Int64
	RequestTimeoutCount atomic.Int64
	FallbackToV2Count  atomic.Int64
}

// Snapshot returns a point-in-time copy of the metrics for reporting.
func (m *SessionMetrics) Snapshot() SessionMetricsSnapshot {
	return SessionMetricsSnapshot{
		HandshakeCount:      m.HandshakeCount.Load(),
		HandshakeDurationNs: m.HandshakeDurationNs.Load(),
		SessionReuseCount:   m.SessionReuseCount.Load(),
		ReconnectCount:      m.ReconnectCount.Load(),
		RequestTimeoutCount: m.RequestTimeoutCount.Load(),
		FallbackToV2Count:   m.FallbackToV2Count.Load(),
	}
}

// SessionMetricsSnapshot is an immutable copy of SessionMetrics values.
type SessionMetricsSnapshot struct {
	HandshakeCount      int64 `json:"handshakeCount"`
	HandshakeDurationNs int64 `json:"handshakeDurationNs"`
	SessionReuseCount   int64 `json:"sessionReuseCount"`
	ReconnectCount      int64 `json:"reconnectCount"`
	RequestTimeoutCount int64 `json:"requestTimeoutCount"`
	FallbackToV2Count   int64 `json:"fallbackToV2Count"`
}

// AvgHandshakeDurationNs returns the average handshake duration, or 0 if no handshakes.
func (s SessionMetricsSnapshot) AvgHandshakeDurationNs() int64 {
	if s.HandshakeCount == 0 {
		return 0
	}
	return s.HandshakeDurationNs / s.HandshakeCount
}
