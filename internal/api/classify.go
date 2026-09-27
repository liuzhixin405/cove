package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrorKind is the coarse nature of a failed model call, for the diagnostic
// layer and the UI. It replaces matching error text in those layers: the
// status carried by StatusError/RetryableError and the typed errors below
// decide, and the textual fallbacks live here only.
type ErrorKind int

const (
	KindUnknown ErrorKind = iota
	KindCanceled
	KindContextLength
	KindRateLimit
	KindAuth
	KindBadRequest
	KindServerError
	KindTransport
	KindToolArgs
	KindProviderUnavailable
	// KindUnreachable: the server could not be reached at all (connection
	// refused, unknown host); KindTimeout: it did not answer in time.
	KindUnreachable
	KindTimeout
)

func (k ErrorKind) String() string {
	switch k {
	case KindCanceled:
		return "canceled"
	case KindContextLength:
		return "context_length"
	case KindRateLimit:
		return "rate_limit"
	case KindAuth:
		return "auth"
	case KindBadRequest:
		return "bad_request"
	case KindServerError:
		return "server_error"
	case KindTransport:
		return "transport"
	case KindToolArgs:
		return "tool_args"
	case KindProviderUnavailable:
		return "provider_unavailable"
	case KindUnreachable:
		return "unreachable"
	case KindTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// ToolArgsInvalidError says a model's tool call carried arguments that were
// not JSON even after RepairToolArguments; the engine reports it so repeated
// occurrences with one model can be counted.
type ToolArgsInvalidError struct{ Tool string }

func (e *ToolArgsInvalidError) Error() string {
	return "tool call arguments for " + e.Tool + " were not valid JSON"
}

// ProviderUnavailableError says the fallback chain marked a provider
// unavailable after repeated failures; Cause is the failure that tipped it.
type ProviderUnavailableError struct {
	Provider string
	Fails    int
	Cause    error
}

func (e *ProviderUnavailableError) Error() string {
	return "provider " + e.Provider + " marked unavailable: " + e.Cause.Error()
}

func (e *ProviderUnavailableError) Unwrap() error { return e.Cause }

// Classify rates err. The typed errors come first, so a provider marked
// unavailable because of a context overflow is KindProviderUnavailable, and
// a cancellation is never mistaken for a transport error.
func Classify(err error) ErrorKind {
	if err == nil {
		return KindUnknown
	}
	var pu *ProviderUnavailableError
	if errors.As(err, &pu) {
		return KindProviderUnavailable
	}
	var ta *ToolArgsInvalidError
	if errors.As(err, &ta) {
		return KindToolArgs
	}
	if errors.Is(err, context.Canceled) {
		return KindCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	if IsContextLengthError(err) {
		return KindContextLength
	}
	switch st := statusOf(err); {
	case st == http.StatusTooManyRequests:
		return KindRateLimit
	case st == http.StatusUnauthorized || st == http.StatusForbidden:
		return KindAuth
	case st == http.StatusBadRequest:
		return KindBadRequest
	case st >= 500:
		return KindServerError
	case st != 0:
		return KindUnknown
	}
	s := strings.ToLower(err.Error())
	switch {
	case isRateLimit(err):
		return KindRateLimit
	case isPermanent(err):
		return KindAuth
	case strings.Contains(s, "connection refused"), strings.Contains(s, "no such host"),
		strings.Contains(s, "network is unreachable"), strings.Contains(s, "actively refused"):
		return KindUnreachable
	case strings.Contains(s, "timeout"), strings.Contains(s, "timed out"), strings.Contains(s, "deadline exceeded"):
		return KindTimeout
	case isTemporary(err):
		return KindTransport
	}
	return KindUnknown
}
