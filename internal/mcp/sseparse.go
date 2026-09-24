package mcp

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one dispatched Server-Sent Event.
type sseEvent struct {
	Type string // "" when the stream gave no event: field ("message" by default)
	Data string
}

// readSSE parses an event stream and calls onEvent for every complete event,
// until the body ends or onEvent returns false.
//
// It follows the SSE spec's line rules rather than splitting on "\n\n": both
// transports used to do that and then accept only events that STARTED with
// "data: ". Real servers (the official SDKs among them) send "event: message"
// first, may use CRLF line endings, split data over several lines, or omit the
// space after the colon - every one of those events was silently dropped, and
// with it the JSON-RPC response a caller was waiting on.
func readSSE(body io.Reader, onEvent func(sseEvent) bool) error {
	r := bufio.NewReader(body)
	var typ string
	var data []string
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 || err == nil {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if len(data) > 0 {
					if !onEvent(sseEvent{Type: typ, Data: strings.Join(data, "\n")}) {
						return nil
					}
				}
				typ, data = "", nil
			case strings.HasPrefix(line, ":"):
				// Comment / keepalive.
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					typ = value
				case "data":
					data = append(data, value)
				}
			}
		}
		if err != nil {
			return err
		}
	}
}

// isMessageEvent reports whether an event carries a JSON-RPC message.
func isMessageEvent(ev sseEvent) bool {
	return ev.Type == "" || ev.Type == "message"
}
