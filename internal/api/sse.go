package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// sseDataPayload returns the payload of a server-sent-events "data" line.
//
// The space after "data:" is optional in the SSE format and several
// OpenAI-compatible gateways leave it out; matching only "data: " used to
// drop every line of such a stream, so the answer came back empty. Comments
// (": keep-alive"), blank lines and other fields (event:, id:, retry:)
// report ok=false. A bare JSON object line is accepted too, for servers that
// stream newline-delimited JSON instead of SSE.
func sseDataPayload(line string) (payload string, ok bool) {
	line = strings.TrimSpace(line) // also drops the \r of CRLF streams
	if strings.HasPrefix(line, "data:") {
		return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
	}
	if strings.HasPrefix(line, "{") {
		return line, true
	}
	return "", false
}

// streamErrorText renders an in-stream error object for the error message.
func streamErrorText(e *oaiStreamError) string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = "unknown error"
	}
	if e.Type != "" {
		msg = e.Type + ": " + msg
	}
	return truncate(msg, 500)
}

// streamError is the error for a failure the provider reported inside a
// 200 stream (Anthropic's "error" event, an OpenAI-compatible
// {"error":{...}} chunk).
//
// It carries the HTTP status the same failure would have had before the
// header, typed exactly as endpoint.post types it: a *RetryableError for
// 5xx/429, a *StatusError otherwise. It used to be a plain fmt.Errorf, so
// Classify said KindUnknown and isTemporary said false: a streamed
// overloaded_error neither fired the engine's fallback nor cooled the
// provider down, while the identical HTTP 529 did both. An error object of
// unknown type keeps its text and carries no status.
func streamError(e *oaiStreamError) error {
	if e == nil {
		e = &oaiStreamError{}
	}
	msg := "provider stream error: " + streamErrorText(e)
	status := streamErrorStatus(e)
	switch {
	case status == 0:
		// Unknown type: reported as text, classified by the textual
		// heuristics only rather than under an invented status.
		return errors.New(msg)
	case status >= 500 || status == http.StatusTooManyRequests:
		return &RetryableError{Status: status, Msg: msg}
	default:
		return &StatusError{Status: status, Msg: msg}
	}
}

// streamErrorStatus maps an in-stream error object to the HTTP status the
// provider would have used for it before the header: a numeric code as-is
// (OpenAI-compatible gateways put the upstream status there), else the
// Anthropic / OpenAI error type. 0 means unknown.
func streamErrorStatus(e *oaiStreamError) int {
	if st := numericCode(e.Code); st >= 400 && st <= 599 {
		return st
	}
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "overloaded_error", "overloaded":
		return 529 // Anthropic's overloaded status
	case "rate_limit_error", "rate_limit_exceeded", "rate_limit":
		return http.StatusTooManyRequests
	case "api_error", "server_error", "internal_error", "internal_server_error":
		return http.StatusInternalServerError
	case "authentication_error", "invalid_api_key":
		return http.StatusUnauthorized
	case "permission_error", "permission_denied":
		return http.StatusForbidden
	case "invalid_request_error", "invalid_request", "bad_request":
		return http.StatusBadRequest
	case "not_found_error", "not_found":
		return http.StatusNotFound
	case "request_too_large":
		return http.StatusRequestEntityTooLarge
	}
	return 0
}

// numericCode reads a JSON "code" that is a number, or a string holding
// one; anything else (OpenAI's "insufficient_quota") is 0.
func numericCode(code any) int {
	switch v := code.(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	case int:
		return v
	}
	return 0
}
