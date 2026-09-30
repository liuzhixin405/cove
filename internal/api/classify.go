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

// Classify rates err. The typed errors come first, so a cancellation is
// never mistaken for a transport error.
func Classify(err error) ErrorKind {
	if err == nil {
		return KindUnknown
	}
	var ta *ToolArgsInvalidError
	if errors.As(err, &ta) {
		return KindToolArgs
	}
	if errors.Is(err, context.Canceled) {
		return KindCanceled
	}
	// The idle watchdog's stall is a timeout like a deadline; it used to fall
	// through to KindUnknown and was reported and retried as neither.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStreamStalled) {
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

func isRateLimit(err error) bool {
	if st := statusOf(err); st != 0 {
		return st == http.StatusTooManyRequests
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many requests")
}

func isTemporary(err error) bool {
	// A stalled stream is a timeout of the body: transient, so the fallback
	// cools the provider down. Its text carries neither a status nor
	// "timeout", so three stalls used to mark the provider unavailable.
	if errors.Is(err, ErrStreamStalled) {
		return true
	}
	if st := statusOf(err); st != 0 {
		return st >= 500
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "timeout") ||
		strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "eof") ||
		strings.Contains(s, "temporary")
}

func isPermanent(err error) bool {
	if st := statusOf(err); st != 0 {
		return st == http.StatusUnauthorized || st == http.StatusForbidden
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid api key") ||
		strings.Contains(s, "authentication")
}
