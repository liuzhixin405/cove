package api

import "strings"

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
