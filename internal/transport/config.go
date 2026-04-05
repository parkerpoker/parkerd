package transport

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// SessionConfig holds timeout and keepalive knobs for the v3 session layer.
// Values are resolved from environment variables, then config flags, then defaults.
type SessionConfig struct {
	ConnectTimeout   time.Duration
	HandshakeTimeout time.Duration
	RequestTimeout   time.Duration
	IdleTimeout      time.Duration
	KeepaliveInterval time.Duration
}

// Direct defaults (non-Tor).
var defaultDirectConfig = SessionConfig{
	ConnectTimeout:    5 * time.Second,
	HandshakeTimeout:  5 * time.Second,
	RequestTimeout:    8 * time.Second,
	IdleTimeout:       120 * time.Second,
	KeepaliveInterval: 30 * time.Second,
}

// Tor defaults (higher latency budget).
var defaultTorConfig = SessionConfig{
	ConnectTimeout:    12 * time.Second,
	HandshakeTimeout:  12 * time.Second,
	RequestTimeout:    20 * time.Second,
	IdleTimeout:       120 * time.Second,
	KeepaliveInterval: 30 * time.Second,
}

// DefaultSessionConfig returns the default session config for the given transport mode.
func DefaultSessionConfig(isTor bool) SessionConfig {
	if isTor {
		return defaultTorConfig
	}
	return defaultDirectConfig
}

// ResolveSessionConfig builds a SessionConfig by reading PARKER_PEER_TRANSPORT_*
// environment variables, falling back to defaults for the given mode.
func ResolveSessionConfig(isTor bool) SessionConfig {
	defaults := DefaultSessionConfig(isTor)
	return SessionConfig{
		ConnectTimeout:    envDurationMS("PARKER_PEER_TRANSPORT_CONNECT_TIMEOUT_MS", defaults.ConnectTimeout),
		HandshakeTimeout:  envDurationMS("PARKER_PEER_TRANSPORT_HANDSHAKE_TIMEOUT_MS", defaults.HandshakeTimeout),
		RequestTimeout:    envDurationMS("PARKER_PEER_TRANSPORT_REQUEST_TIMEOUT_MS", defaults.RequestTimeout),
		IdleTimeout:       envDurationMS("PARKER_PEER_TRANSPORT_IDLE_TIMEOUT_MS", defaults.IdleTimeout),
		KeepaliveInterval: envDurationMS("PARKER_PEER_TRANSPORT_KEEPALIVE_MS", defaults.KeepaliveInterval),
	}
}

func envDurationMS(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}
