package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

// specStreamableServer behaves like the official SDKs' Streamable HTTP server
// (spec 2025-03-26): every POST is answered on its own response - as a plain
// application/json body or as an SSE stream - the session id is issued on the
// initialize response and required afterwards, and the optional GET stream is
// refused (405 in stateless mode, 400 without a session).
type specStreamableServer struct {
	srv *httptest.Server

	sseReplies bool          // answer requests with text/event-stream bodies
	crlf       bool          // use CRLF line endings in SSE bodies
	callDelay  time.Duration // how long tools/call takes before any byte is sent

	mu       sync.Mutex
	sessions []string // Mcp-Session-Id seen on each POST after initialize
}

func newSpecStreamableServer(t *testing.T, configure func(*specStreamableServer)) *specStreamableServer {
	t.Helper()
	s := &specStreamableServer{}
	if configure != nil {
		configure(s)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *specStreamableServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if r.Header.Get("Mcp-Session-Id") == "" {
			http.Error(w, "Bad Request: Mcp-Session-Id header is required", http.StatusBadRequest)
			return
		}
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)

	if req.Method != "initialize" {
		sid := r.Header.Get("Mcp-Session-Id")
		s.mu.Lock()
		s.sessions = append(s.sessions, sid)
		s.mu.Unlock()
		if sid != "sess-xyz" {
			http.Error(w, "Bad Request: No valid session ID provided", http.StatusBadRequest)
			return
		}
	}
	if len(req.ID) == 0 { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var result string
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "sess-xyz")
		result = `{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"spec","version":"1"}}`
	case "tools/list":
		result = `{"tools":[{"name":"slow","inputSchema":{"type":"object"}}]}`
	case "tools/call":
		result = `{"content":[{"type":"text","text":"done"}]}`
	default:
		result = `{}`
	}
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)

	flusher, _ := w.(http.Flusher)
	if !s.sseReplies {
		if req.Method == "tools/call" {
			time.Sleep(s.callDelay)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, msg)
		return
	}
	nl := "\n"
	if s.crlf {
		nl = "\r\n"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if req.Method == "tools/call" {
		// A keepalive comment first, then the real event after the delay.
		_, _ = io.WriteString(w, ": keepalive"+nl+nl)
		flusher.Flush()
		time.Sleep(s.callDelay)
	}
	_, _ = io.WriteString(w, "event: message"+nl+"id: 1"+nl+"data: "+msg+nl+nl)
	flusher.Flush()
}

func (s *specStreamableServer) seenSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sessions...)
}

// connectSpec runs the full client stack against the fake server and returns
// the result of a tools/call.
func connectSpec(t *testing.T, s *specStreamableServer) (*CallToolResult, error) {
	t.Helper()
	tr, err := NewStreamableHTTPTransport(s.srv.URL + "/mcp")
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	c := NewClient(tr)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	if len(tools) != 1 || tools[0].Name != "slow" {
		return nil, fmt.Errorf("tools = %+v", tools)
	}
	return c.CallTool(ctx, "slow", nil)
}

func requireDone(t *testing.T, res *CallToolResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "done" {
		t.Fatalf("tools/call result = %+v; want done", res.Content)
	}
}

// TestStreamableHTTP_JSONResponses: a server may answer each POST with a plain
// application/json body. That body IS the JSON-RPC response; it used to be
// closed unread, so initialize never completed against such servers.
func TestStreamableHTTP_JSONResponses(t *testing.T) {
	s := newSpecStreamableServer(t, nil)
	res, err := connectSpec(t, s)
	requireDone(t, res, err)
}

// TestStreamableHTTP_SendsSessionID: the session id issued on the initialize
// response must accompany every later request, or the server rejects them.
func TestStreamableHTTP_SendsSessionID(t *testing.T) {
	s := newSpecStreamableServer(t, nil)
	res, err := connectSpec(t, s)
	requireDone(t, res, err)
	for i, sid := range s.seenSessions() {
		if sid != "sess-xyz" {
			t.Fatalf("request #%d after initialize carried session %q; want sess-xyz", i, sid)
		}
	}
}

// TestStreamableHTTP_SSEEventsWithTypeAndCRLF: SDK servers send
// "event: message" before "data:", and SSE allows CRLF line endings. The old
// parser only accepted events that STARTED with "data: " and split on "\n\n",
// so every such response was dropped.
func TestStreamableHTTP_SSEEventsWithTypeAndCRLF(t *testing.T) {
	for _, crlf := range []bool{false, true} {
		s := newSpecStreamableServer(t, func(s *specStreamableServer) {
			s.sseReplies = true
			s.crlf = crlf
		})
		res, err := connectSpec(t, s)
		if err != nil {
			t.Fatalf("crlf=%v: %v", crlf, err)
		}
		requireDone(t, res, err)
	}
}

// TestStreamableHTTP_LongToolCallIsNotCutOff: a tool call may legitimately run
// for minutes. The POST used a client with a fixed overall timeout, which also
// covers reading the body, so a long call's response was cut off mid-stream
// and the caller waited for a reply that could no longer arrive. Only the
// caller's context may bound a request.
func TestStreamableHTTP_LongToolCallIsNotCutOff(t *testing.T) {
	old := httpPostTimeout
	httpPostTimeout = 200 * time.Millisecond
	defer func() { httpPostTimeout = old }()

	for _, sse := range []bool{false, true} {
		s := newSpecStreamableServer(t, func(s *specStreamableServer) {
			s.sseReplies = sse
			s.callDelay = 700 * time.Millisecond
		})
		res, err := connectSpec(t, s)
		if err != nil {
			t.Fatalf("sse=%v: %v", sse, err)
		}
		requireDone(t, res, err)
	}
}

// TestStreamableHTTP_CloseDoesNotWarn: shutting the transport down cancels its
// own stream, which is not an error. It used to log a WARN ("read error:
// context canceled"), which the UI surfaces to the user on every /mcp
// disconnect and at exit.
func TestStreamableHTTP_CloseDoesNotWarn(t *testing.T) {
	var mu sync.Mutex
	var warnings []string
	log.AddSink(func(level log.Level, msg string) {
		mu.Lock()
		warnings = append(warnings, msg)
		mu.Unlock()
	})
	defer log.ClearSinks()

	// Whether the reader sees "context canceled" or a closed body depends on
	// timing, so shut down several times.
	for i := 0; i < 30; i++ {
		s := newStreamableTestServer(t)
		tr, err := NewStreamableHTTPTransport(s.srv.URL)
		if err != nil {
			t.Fatalf("NewStreamableHTTPTransport: %v", err)
		}
		<-s.streamOpen
		_ = tr.Close()
		requireNoGoroutine(t, ".readStream")
		s.srv.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	for _, w := range warnings {
		if strings.Contains(w, "streamablehttp") {
			t.Fatalf("Close logged a warning: %q", w)
		}
	}
}
