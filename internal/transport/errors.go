package transport

import (
	"errors"
	"fmt"
)

// TransportErrorKind classifies transport-level failures so callers can
// distinguish retryable timeouts from permanent session or version errors.
type TransportErrorKind int

const (
	ErrKindRequestTimeout TransportErrorKind = iota + 1
	ErrKindSessionReset
	ErrKindWireDowngrade
	ErrKindHandshakeFailed
	ErrKindSessionClosed
)

// TransportError is a typed error returned by the v3 session layer.
type TransportError struct {
	Kind    TransportErrorKind
	PeerURL string
	Detail  string
	Cause   error
}

func (e *TransportError) Error() string {
	msg := transportErrorKindLabel(e.Kind)
	if e.PeerURL != "" {
		msg += " peer=" + e.PeerURL
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *TransportError) Unwrap() error { return e.Cause }

func transportErrorKindLabel(kind TransportErrorKind) string {
	switch kind {
	case ErrKindRequestTimeout:
		return "transport request timeout"
	case ErrKindSessionReset:
		return "transport session reset"
	case ErrKindWireDowngrade:
		return "transport wire version downgrade"
	case ErrKindHandshakeFailed:
		return "transport handshake failed"
	case ErrKindSessionClosed:
		return "transport session closed"
	default:
		return fmt.Sprintf("transport error (kind=%d)", kind)
	}
}

// IsTransportTimeout returns true if the error is a transport request timeout.
func IsTransportTimeout(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && te.Kind == ErrKindRequestTimeout
}

// IsSessionReset returns true if the error is a session reset.
func IsSessionReset(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && te.Kind == ErrKindSessionReset
}

// IsWireDowngrade returns true if the error is a wire version downgrade.
func IsWireDowngrade(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && te.Kind == ErrKindWireDowngrade
}

// IsSessionClosed returns true if the error is a session closed error.
func IsSessionClosed(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && te.Kind == ErrKindSessionClosed
}
