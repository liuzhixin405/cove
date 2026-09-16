package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitForCond spins (yielding, never sleeping) until cond holds. It is used to
// observe a state the runtime reaches on its own, e.g. "the transport's message
// buffer is full", so the tests need no arbitrary delays.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("timed out waiting for %s", what)
}

// requireNoGoroutine fails if any goroutine is still executing the named
// function, which is how a reader goroutine that blocked instead of exiting on
// shutdown shows up.
func requireNoGoroutine(t *testing.T, fn string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutine profile: %v", err)
		}
		last = buf.String()
		if !strings.Contains(last, fn+"+") && !strings.Contains(last, fn+"(") {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("a goroutine is still parked in %s after the transport was closed; it did not exit on shutdown", fn)
}

// ---------------------------------------------------------------------------
// SSE transport
// ---------------------------------------------------------------------------

type sseTestServer struct {
	srv       *httptest.Server
	sessionID string

	events     chan string
	streamOpen chan struct{}
	streamGone chan struct{}
	goneOnce   sync.Once
	posts      chan json.RawMessage
	postQuery  chan string

	postStatus  atomic.Int32
	omitSession atomic.Bool
}

func newSSETestServer(t *testing.T) *sseTestServer {
	t.Helper()
	s := &sseTestServer{
		sessionID:  "sess-42",
		events:     make(chan string, 1024),
		streamOpen: make(chan struct{}, 4),
		streamGone: make(chan struct{}),
		posts:      make(chan json.RawMessage, 32),
		postQuery:  make(chan string, 32),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if s.omitSession.Load() {
			io.WriteString(w, `{}`)
			return
		}
		fmt.Fprintf(w, `{"sessionId":%q}`, s.sessionID)
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("response writer is not a Flusher")
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			select {
			case s.streamOpen <- struct{}{}:
			default:
			}
			for {
				select {
				case ev := <-s.events:
					fmt.Fprintf(w, "data: %s\n\n", ev)
					flusher.Flush()
				case <-r.Context().Done():
					s.goneOnce.Do(func() { close(s.streamGone) })
					return
				}
			}
		}

		body, _ := io.ReadAll(r.Body)
		if code := int(s.postStatus.Load()); code >= 400 {
			w.WriteHeader(code)
			io.WriteString(w, "session expired")
			return
		}
		select {
		case s.posts <- json.RawMessage(body):
			s.postQuery <- r.URL.RawQuery
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})

	s.srv = httptest.NewServer(mux)
	return s
}

func TestNewSSETransport_RequiresSessionID(t *testing.T) {
	s := newSSETestServer(t)
	defer s.srv.Close()
	s.omitSession.Store(true)

	tr, err := NewSSETransport(s.srv.URL)
	if err == nil {
		tr.Close()
		t.Fatal("NewSSETransport succeeded without a session id")
	}
	if !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("error = %q; want it to mention the missing session ID", err)
	}
}

func TestNewSSETransport_ReportsConnectFailure(t *testing.T) {
	s := newSSETestServer(t)
	url := s.srv.URL
	s.srv.Close() // nothing is listening any more

	tr, err := NewSSETransport(url)
	if err == nil {
		tr.Close()
		t.Fatal("NewSSETransport succeeded against a dead server")
	}
	if !strings.Contains(err.Error(), "sse connect") {
		t.Fatalf("error = %q; want it wrapped as an sse connect failure", err)
	}
}

func TestSSETransport_SendPostsToSessionAndSurfacesHTTPErrors(t *testing.T) {
	s := newSSETestServer(t)
	defer s.srv.Close()

	tr, err := NewSSETransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewSSETransport: %v", err)
	}
	defer tr.Close()

	req := Request{JSONRPC: JSONRPC{Jsonrpc: "2.0"}, ID: 3, Method: "tools/list"}
	if err := tr.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case body := <-s.posts:
		var got Request
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("server received unparsable body %q: %v", body, err)
		}
		if got.ID != 3 || got.Method != "tools/list" {
			t.Fatalf("server received %+v; want id 3 tools/list", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the posted message")
	}
	if q := <-s.postQuery; !strings.Contains(q, "sessionId="+s.sessionID) {
		t.Fatalf("post query = %q; want the session id", q)
	}

	// A non-2xx means the message was NOT delivered; Send must say so rather
	// than leaving the caller waiting for a response that will never come.
	s.postStatus.Store(http.StatusGone)
	err = tr.Send(context.Background(), req)
	if err == nil {
		t.Fatal("Send returned nil for an HTTP 410 response")
	}
	if !strings.Contains(err.Error(), "410") || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("error = %q; want the status code and body", err)
	}
}

// TestSSETransport_BackpressureDoesNotDropMessages: the reader must block when
// the message channel is full instead of discarding events. A dropped JSON-RPC
// response leaves its caller hanging until the request times out.
func TestSSETransport_BackpressureDoesNotDropMessages(t *testing.T) {
	s := newSSETestServer(t)
	defer s.srv.Close()

	tr, err := NewSSETransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewSSETransport: %v", err)
	}
	defer tr.Close()

	select {
	case <-s.streamOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream was never opened")
	}

	total := cap(tr.msgChan) * 4
	for i := 0; i < total; i++ {
		s.events <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%d}`, i, i)
	}

	// Let the reader run ahead until its buffer is saturated, so the remaining
	// events can only survive if the reader applies backpressure.
	waitForCond(t, "the transport message buffer to fill up", func() bool {
		return len(tr.msgChan) == cap(tr.msgChan)
	})

	for i := 0; i < total; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		msg, err := tr.Receive(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Receive #%d: %v (messages were dropped on a full channel)", i, err)
		}
		var got struct {
			ID     int `json:"id"`
			Result int `json:"result"`
		}
		if err := json.Unmarshal(msg, &got); err != nil {
			t.Fatalf("Receive #%d: bad payload %q: %v", i, msg, err)
		}
		if got.ID != i {
			t.Fatalf("Receive #%d returned id %d: %d message(s) were dropped", i, got.ID, got.ID-i)
		}
	}
}

// TestSSETransport_ReaderExitsOnShutdown: with the reader parked on a full
// channel, Close must unblock it, tear the HTTP stream down and make Receive
// report the shutdown.
func TestSSETransport_ReaderExitsOnShutdown(t *testing.T) {
	s := newSSETestServer(t)
	defer s.srv.Close()

	tr, err := NewSSETransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewSSETransport: %v", err)
	}

	select {
	case <-s.streamOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream was never opened")
	}

	for i := 0; i < cap(tr.msgChan)*2; i++ {
		s.events <- fmt.Sprintf(`{"id":%d}`, i)
	}
	waitForCond(t, "the transport message buffer to fill up", func() bool {
		return len(tr.msgChan) == cap(tr.msgChan)
	})

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-s.streamGone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not tear down the SSE stream request")
	}
	requireNoGoroutine(t, ".listenSSE")

	// Receive must eventually report EOF: buffered messages may still drain
	// first, but it must not block forever.
	sawEOF := false
	for i := 0; i < cap(tr.msgChan)+2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := tr.Receive(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("Receive blocked after Close instead of reporting the shutdown")
		}
		if err != nil {
			sawEOF = true
			break
		}
	}
	if !sawEOF {
		t.Fatal("Receive never reported the closed transport")
	}

	// Close is reachable from several paths and must stay safe.
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSSETransport_ReceiveHonoursCallerContext(t *testing.T) {
	s := newSSETestServer(t)
	defer s.srv.Close()

	tr, err := NewSSETransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewSSETransport: %v", err)
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := tr.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive error = %v; want context.DeadlineExceeded", err)
	}
}

// ---------------------------------------------------------------------------
// Streamable HTTP transport
// ---------------------------------------------------------------------------

type streamableTestServer struct {
	srv *httptest.Server

	events     chan string
	streamOpen chan struct{}
	streamGone chan struct{}
	goneOnce   sync.Once
	posts      chan json.RawMessage

	getStatus   atomic.Int32
	postStatus  atomic.Int32
	postStreams atomic.Bool // answer POSTs with an SSE body
	postEvents  chan string
}

func newStreamableTestServer(t *testing.T) *streamableTestServer {
	t.Helper()
	s := &streamableTestServer{
		events:     make(chan string, 1024),
		streamOpen: make(chan struct{}, 4),
		streamGone: make(chan struct{}),
		posts:      make(chan json.RawMessage, 32),
		postEvents: make(chan string, 32),
	}

	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		if r.Method == http.MethodGet {
			if code := int(s.getStatus.Load()); code >= 400 {
				w.WriteHeader(code)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			select {
			case s.streamOpen <- struct{}{}:
			default:
			}
			for {
				select {
				case ev := <-s.events:
					fmt.Fprintf(w, "data: %s\n\n", ev)
					flusher.Flush()
				case <-r.Context().Done():
					s.goneOnce.Do(func() { close(s.streamGone) })
					return
				}
			}
		}

		body, _ := io.ReadAll(r.Body)
		if code := int(s.postStatus.Load()); code >= 400 {
			w.WriteHeader(code)
			io.WriteString(w, "upstream refused")
			return
		}
		select {
		case s.posts <- json.RawMessage(body):
		default:
		}
		if s.postStreams.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			for {
				select {
				case ev := <-s.postEvents:
					fmt.Fprintf(w, "data: %s\n\n", ev)
					flusher.Flush()
					if ev == "__end__" {
						return
					}
				case <-r.Context().Done():
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	return s
}

func TestNewStreamableHTTPTransport_RejectsErrorStatus(t *testing.T) {
	s := newStreamableTestServer(t)
	defer s.srv.Close()
	s.getStatus.Store(http.StatusNotFound)

	tr, err := NewStreamableHTTPTransport(s.srv.URL)
	if err == nil {
		tr.Close()
		t.Fatal("NewStreamableHTTPTransport accepted a 404 stream")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %q; want the status code", err)
	}
}

func TestStreamableHTTPTransport_SendSurfacesHTTPErrors(t *testing.T) {
	s := newStreamableTestServer(t)
	defer s.srv.Close()

	tr, err := NewStreamableHTTPTransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewStreamableHTTPTransport: %v", err)
	}
	defer tr.Close()

	req := Request{JSONRPC: JSONRPC{Jsonrpc: "2.0"}, ID: 8, Method: "tools/call"}
	if err := tr.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case body := <-s.posts:
		var got Request
		if err := json.Unmarshal(body, &got); err != nil || got.ID != 8 || got.Method != "tools/call" {
			t.Fatalf("server received %q (parsed %+v, err %v)", body, got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the posted message")
	}

	s.postStatus.Store(http.StatusInternalServerError)
	err = tr.Send(context.Background(), req)
	if err == nil {
		t.Fatal("Send returned nil for an HTTP 500 response")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "upstream refused") {
		t.Fatalf("error = %q; want the status code and body", err)
	}
}

// TestStreamableHTTPTransport_SendConsumesStreamedResponse: when the POST is
// answered with an SSE body, the messages in it must reach Receive. Closing the
// body in Send would truncate them.
func TestStreamableHTTPTransport_SendConsumesStreamedResponse(t *testing.T) {
	s := newStreamableTestServer(t)
	defer s.srv.Close()

	tr, err := NewStreamableHTTPTransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewStreamableHTTPTransport: %v", err)
	}
	defer tr.Close()

	s.postStreams.Store(true)
	if err := tr.Send(context.Background(), Request{JSONRPC: JSONRPC{Jsonrpc: "2.0"}, ID: 1, Method: "tools/list"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-s.posts

	s.postEvents <- `{"jsonrpc":"2.0","id":1,"result":"streamed"}`

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg, err := tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	var got struct {
		ID     int    `json:"id"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatalf("payload %q: %v", msg, err)
	}
	if got.ID != 1 || got.Result != "streamed" {
		t.Fatalf("received %+v; want id 1 result streamed", got)
	}
	s.postEvents <- "__end__"
}

func TestStreamableHTTPTransport_BackpressureDoesNotDropMessages(t *testing.T) {
	s := newStreamableTestServer(t)
	defer s.srv.Close()

	tr, err := NewStreamableHTTPTransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewStreamableHTTPTransport: %v", err)
	}
	defer tr.Close()

	select {
	case <-s.streamOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("stream was never opened")
	}

	total := cap(tr.msgChan) * 4
	for i := 0; i < total; i++ {
		s.events <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%d}`, i)
	}
	waitForCond(t, "the transport message buffer to fill up", func() bool {
		return len(tr.msgChan) == cap(tr.msgChan)
	})

	for i := 0; i < total; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		msg, err := tr.Receive(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Receive #%d: %v (messages were dropped on a full channel)", i, err)
		}
		var got struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(msg, &got); err != nil {
			t.Fatalf("Receive #%d: bad payload %q", i, msg)
		}
		if got.ID != i {
			t.Fatalf("Receive #%d returned id %d: %d message(s) were dropped", i, got.ID, got.ID-i)
		}
	}
}

func TestStreamableHTTPTransport_ReaderExitsOnShutdown(t *testing.T) {
	s := newStreamableTestServer(t)
	defer s.srv.Close()

	tr, err := NewStreamableHTTPTransport(s.srv.URL)
	if err != nil {
		t.Fatalf("NewStreamableHTTPTransport: %v", err)
	}

	select {
	case <-s.streamOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("stream was never opened")
	}
	for i := 0; i < cap(tr.msgChan)*2; i++ {
		s.events <- fmt.Sprintf(`{"id":%d}`, i)
	}
	waitForCond(t, "the transport message buffer to fill up", func() bool {
		return len(tr.msgChan) == cap(tr.msgChan)
	})

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-s.streamGone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not tear down the streaming request")
	}
	requireNoGoroutine(t, ".readStream")

	tr.mu.Lock()
	stream := tr.stream
	tr.mu.Unlock()
	if stream != nil {
		t.Fatal("Close left the stream body attached")
	}

	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestPool_ConnectDoesNotHoldLockDuringHandshake drives a real SSE server that
// accepts the connection and then never answers initialize. While server A is
// stuck in its handshake, unrelated pool operations must still complete.
func TestPool_ConnectDoesNotHoldLockDuringHandshake(t *testing.T) {
	s := newSSETestServer(t)
	defer s.srv.Close()

	p := NewPool()
	p.servers["B"] = managedWithTransport("B", newScriptedTransport(), []Tool{{Name: "b-tool"}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectDone := make(chan error, 1)
	go func() {
		connectDone <- p.Connect(ctx, "A", ServerConfig{Type: "sse", URL: s.srv.URL})
	}()

	// The initialize POST proves Connect is inside the handshake.
	select {
	case body := <-s.posts:
		if !strings.Contains(string(body), "initialize") {
			t.Fatalf("first posted message = %q; want initialize", body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Connect never sent initialize")
	}

	type outcome struct {
		servers, tools int
	}
	opDone := make(chan outcome, 1)
	go func() {
		o := outcome{servers: len(p.AllServers()), tools: len(p.AllTools())}
		p.Disconnect("B")
		opDone <- o
	}()

	select {
	case o := <-opDone:
		if o.servers != 1 || o.tools != 1 {
			t.Fatalf("mid-handshake reads saw %d servers / %d tools; want 1/1", o.servers, o.tools)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AllServers/AllTools/Disconnect blocked while another server was handshaking: Connect holds the pool lock across the handshake")
	}

	if _, ok := p.servers["B"]; ok {
		t.Fatal("Disconnect did not remove server B")
	}

	cancel()
	select {
	case err := <-connectDone:
		if err == nil {
			t.Fatal("Connect succeeded although initialize was never answered")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Connect did not return after its context was cancelled")
	}
	if _, ok := p.servers["A"]; ok {
		t.Fatal("a failed Connect registered the server")
	}
}
