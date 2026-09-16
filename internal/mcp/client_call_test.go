package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedTransport is a fully controllable Transport: the test observes every
// outbound message and decides exactly which inbound messages appear, so these
// tests never need a sleep to synchronise with the client's receive loop.
type scriptedTransport struct {
	sent     chan sentMsg
	incoming chan json.RawMessage
	closed   chan struct{}

	mu      sync.Mutex
	sendErr error
	closes  int
}

type sentMsg struct {
	Raw      json.RawMessage
	ID       *int
	Method   string
	Deadline time.Time
	HasDDL   bool
}

func newScriptedTransport() *scriptedTransport {
	return &scriptedTransport{
		sent:     make(chan sentMsg, 32),
		incoming: make(chan json.RawMessage, 32),
		closed:   make(chan struct{}),
	}
}

func (s *scriptedTransport) setSendErr(err error) {
	s.mu.Lock()
	s.sendErr = err
	s.mu.Unlock()
}

func (s *scriptedTransport) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func (s *scriptedTransport) Send(ctx context.Context, msg any) error {
	s.mu.Lock()
	err := s.sendErr
	s.mu.Unlock()

	data, merr := json.Marshal(msg)
	if merr != nil {
		return merr
	}
	var parsed struct {
		ID     *int   `json:"id"`
		Method string `json:"method"`
	}
	_ = json.Unmarshal(data, &parsed)

	m := sentMsg{Raw: data, ID: parsed.ID, Method: parsed.Method}
	m.Deadline, m.HasDDL = ctx.Deadline()
	select {
	case s.sent <- m:
	default:
	}
	return err
}

func (s *scriptedTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	select {
	case m := <-s.incoming:
		return m, nil
	case <-s.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *scriptedTransport) Close() error {
	s.mu.Lock()
	s.closes++
	first := s.closes == 1
	s.mu.Unlock()
	if first {
		close(s.closed)
	}
	return nil
}

func (s *scriptedTransport) nextSent(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-s.sent:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("transport: expected an outbound message, got none")
		return sentMsg{}
	}
}

func respJSON(id int, result string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result))
}

type callResult struct {
	out string
	err error
}

// TestClient_CallCorrelatesResponsesByID answers two in-flight requests in
// REVERSE order; each Call must receive its own result. Correlation by
// arrival order instead of by id would swap them.
func TestClient_CallCorrelatesResponsesByID(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	go c.receiveLoop()
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	firstCh := make(chan callResult, 1)
	go func() {
		var out string
		err := c.Call(ctx, "alpha", nil, &out)
		firstCh <- callResult{out, err}
	}()
	// Observing alpha's Send guarantees its pending entry is registered before
	// beta starts, so the two ids are deterministic.
	alpha := tr.nextSent(t)

	secondCh := make(chan callResult, 1)
	go func() {
		var out string
		err := c.Call(ctx, "beta", nil, &out)
		secondCh <- callResult{out, err}
	}()
	beta := tr.nextSent(t)

	if alpha.Method != "alpha" || beta.Method != "beta" {
		t.Fatalf("outbound methods = %q, %q; want alpha, beta", alpha.Method, beta.Method)
	}
	if alpha.ID == nil || beta.ID == nil {
		t.Fatal("requests must carry an id")
	}
	if *alpha.ID == *beta.ID {
		t.Fatalf("both requests used id %d; ids must be unique", *alpha.ID)
	}

	tr.incoming <- respJSON(*beta.ID, `"beta-result"`)
	tr.incoming <- respJSON(*alpha.ID, `"alpha-result"`)

	got := <-firstCh
	if got.err != nil || got.out != "alpha-result" {
		t.Fatalf("alpha Call = (%q, %v); want (alpha-result, nil)", got.out, got.err)
	}
	got = <-secondCh
	if got.err != nil || got.out != "beta-result" {
		t.Fatalf("beta Call = (%q, %v); want (beta-result, nil)", got.out, got.err)
	}
}

// TestClient_CallTurnsServerErrorIntoGoError: a JSON-RPC error object must
// surface as a Go error carrying the code and message, and must not be
// unmarshalled into the result.
func TestClient_CallTurnsServerErrorIntoGoError(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	go c.receiveLoop()
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	out := "untouched"
	go func() { errCh <- c.Call(ctx, "tools/call", nil, &out) }()

	req := tr.nextSent(t)
	tr.incoming <- json.RawMessage(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Method not found"}}`, *req.ID))

	err := <-errCh
	if err == nil {
		t.Fatal("Call returned nil for a JSON-RPC error response")
	}
	if !strings.Contains(err.Error(), "-32601") || !strings.Contains(err.Error(), "Method not found") {
		t.Fatalf("error = %q; want code and message from the server error", err)
	}
	if out != "untouched" {
		t.Fatalf("result was written on an error response: %q", out)
	}
}

// TestClient_CallAfterCloseFailsFast: once the client is closed there is no
// receive loop left to answer, so Call must report the closed connection
// immediately instead of hanging until the caller's context expires.
func TestClient_CallAfterCloseFailsFast(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	loopDone := make(chan struct{})
	go func() {
		c.receiveLoop()
		close(loopDone)
	}()

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Wait until the receive loop is really gone, so nothing could answer the
	// Call below even in principle.
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("receiveLoop did not exit after Close")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	err := c.Call(ctx, "tools/list", nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Call on a closed client returned nil error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call on a closed client hung until the context deadline instead of failing fast: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Call on a closed client took %v; want an immediate failure", elapsed)
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("error = %q; want it to mention the closed connection", err)
	}
}

// TestClient_CallReturnsOnContextCancellation: a request the server never
// answers must unblock as soon as the caller's context is cancelled.
func TestClient_CallReturnsOnContextCancellation(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	go c.receiveLoop()
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Call(ctx, "never-answered", nil, nil) }()

	tr.nextSent(t) // the request is in flight and registered as pending
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call error = %v; want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Call did not return after its context was cancelled")
	}

	// The abandoned request must not leak a pending entry.
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending table has %d entries after a cancelled Call; want 0", n)
	}
}

// TestClient_CallReturnsOnContextTimeout is the deadline twin of the
// cancellation case: a hung server must not hold a caller past its deadline.
func TestClient_CallReturnsOnContextTimeout(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	go c.receiveLoop()
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Call(ctx, "never-answered", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v; want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Call took %v past a 100ms deadline", elapsed)
	}
}

func TestClient_CallPropagatesSendError(t *testing.T) {
	tr := newScriptedTransport()
	tr.setSendErr(errors.New("broken pipe"))
	c := NewClient(tr)
	go c.receiveLoop()
	defer c.Close()

	err := c.Call(context.Background(), "tools/list", nil, nil)
	if err == nil {
		t.Fatal("Call returned nil when the transport Send failed")
	}
	if !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("error = %q; want the transport error wrapped", err)
	}

	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending table has %d entries after a failed Send; want 0", n)
	}
}

// TestClient_ConnectAppliesHandshakeDeadline verifies Connect bounds the
// initialize round-trip even when the caller passes an unbounded context: the
// context handed to the transport must carry a deadline of handshakeTimeout.
func TestClient_ConnectAppliesHandshakeDeadline(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)

	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()

	req := tr.nextSent(t)
	if req.Method != "initialize" {
		t.Fatalf("first outbound method = %q; want initialize", req.Method)
	}
	if !req.HasDDL {
		t.Fatal("Connect sent initialize with no deadline: an unresponsive server would block Connect forever")
	}
	budget := time.Until(req.Deadline)
	if budget > handshakeTimeout || budget < handshakeTimeout-5*time.Second {
		t.Fatalf("handshake budget = %v; want ~%v", budget, handshakeTimeout)
	}

	// Let Connect finish so the goroutine does not outlive the test. Connect
	// starts the receive loop itself, so the response is picked up here.
	tr.incoming <- respJSON(*req.ID, `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"srv","version":"1.2.3"}}`)
	if err := <-connectDone; err != nil {
		t.Fatalf("Connect: %v", err)
	}
	c.Close()
}

// TestClient_ConnectFailsWhenInitializeNeverAnswered: a transport that accepts
// the connection but never replies must make Connect fail, not hang.
func TestClient_ConnectFailsWhenInitializeNeverAnswered(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Connect(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Connect succeeded without an initialize response")
		}
		if !strings.Contains(err.Error(), "initialize") {
			t.Fatalf("error = %q; want it to name the initialize step", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v; want a wrapped context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect blocked past its handshake deadline")
	}
}

// TestClient_ConnectRecordsServerCapabilities also pins the capability gate on
// ListTools/ListResources: without the advertised capability those must fail
// without a round-trip.
func TestClient_ConnectRecordsServerCapabilities(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	defer c.Close()

	// Connect starts the receive loop itself.
	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()

	req := tr.nextSent(t)
	tr.incoming <- respJSON(*req.ID, `{"protocolVersion":"2024-11-05","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"srv","version":"1.2.3"}}`)

	if err := <-done; err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if info := c.ServerInfo(); info.Name != "srv" || info.Version != "1.2.3" {
		t.Fatalf("ServerInfo = %+v; want {srv 1.2.3}", info)
	}
	if c.serverCaps.Tools == nil || !c.serverCaps.Tools.ListChanged {
		t.Fatalf("server tools capability = %+v; want listChanged true", c.serverCaps.Tools)
	}

	// Connect must also announce itself as initialized.
	notif := tr.nextSent(t)
	if notif.Method != "notifications/initialized" || notif.ID != nil {
		t.Fatalf("after initialize the client sent %q (id=%v); want the notifications/initialized notification", notif.Method, notif.ID)
	}

	// Resources were not advertised: ListResources must fail locally.
	if _, err := c.ListResources(context.Background()); err == nil {
		t.Fatal("ListResources succeeded although the server advertised no resources capability")
	}
	select {
	case m := <-tr.sent:
		t.Fatalf("ListResources performed a round-trip (%q) despite the missing capability", m.Method)
	default:
	}
}

// TestClient_ServerRequestIsNotMisrouted: a message carrying BOTH a method and
// an id is a server-initiated request. It must neither be delivered as a
// notification nor resolve a pending call that happens to share the id.
func TestClient_ServerRequestIsNotMisrouted(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	go c.receiveLoop()
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resCh := make(chan callResult, 1)
	go func() {
		var out string
		err := c.Call(ctx, "tools/list", nil, &out)
		resCh <- callResult{out, err}
	}()
	req := tr.nextSent(t)

	// Messages are processed in order, so the real response is only handled
	// after the server request has been (correctly) ignored.
	tr.incoming <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"sampling/createMessage","params":{}}`, *req.ID))
	tr.incoming <- respJSON(*req.ID, `"real"`)

	got := <-resCh
	if got.err != nil {
		t.Fatalf("Call: %v", got.err)
	}
	if got.out != "real" {
		t.Fatalf("Call result = %q; the server-initiated request was misrouted into the pending table", got.out)
	}
	select {
	case n := <-c.Notifications():
		t.Fatalf("server request %q was delivered as a notification", n.Method)
	default:
	}
}

// TestClient_HandleRawDoesNotBlockWithFullPendingBuffer is a regression test
// for a whole-client deadlock: handleRaw delivered responses while holding
// c.mu, so a second response for an id whose buffer was not drained pinned
// c.mu forever and froze every Call and Close.
func TestClient_HandleRawDoesNotBlockWithFullPendingBuffer(t *testing.T) {
	c := NewClient(newScriptedTransport())

	ch := make(chan *Response, 1)
	c.pending[7] = ch
	ch <- &Response{ID: 7} // buffer already occupied and nobody is draining

	done := make(chan struct{})
	go func() {
		_ = c.handleRaw(respJSON(7, `"second"`))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleRaw blocked delivering a duplicate response while holding c.mu: the client is deadlocked")
	}

	// And the lock must be free afterwards.
	locked := make(chan struct{})
	go func() {
		c.mu.Lock()
		c.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("c.mu is still held after handleRaw returned")
	}
}

// TestClient_ReceiveLoopCleanupDoesNotDeadlock is the same regression on the
// shutdown path: receiveLoop's deferred "transport closed" broadcast pushed
// onto pending channels under c.mu, so an undrained pending channel wedged the
// lock permanently when the transport died.
func TestClient_ReceiveLoopCleanupDoesNotDeadlock(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)

	ch := make(chan *Response, 1)
	c.pending[1] = ch
	ch <- &Response{ID: 1} // a response the caller abandoned before reading

	loopDone := make(chan struct{})
	go func() {
		c.receiveLoop()
		close(loopDone)
	}()

	tr.Close() // Receive returns io.EOF -> receiveLoop unwinds

	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("receiveLoop cleanup blocked on an undrained pending channel while holding c.mu")
	}

	locked := make(chan struct{})
	go func() {
		c.mu.Lock()
		c.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("c.mu is still held after receiveLoop returned")
	}
}

func TestClient_CloseIsIdempotent(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	go c.receiveLoop()

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := tr.closeCount(); got != 2 {
		t.Fatalf("transport Close calls = %d; want 2 (Client.Close must forward every call)", got)
	}
}

// TestClient_ReceiveStripsCRAndTolerantOfBlankLines drives stdioTransport's
// framing directly (no child process) over a canned stream.
func TestStdioTransport_ReceiveFraming(t *testing.T) {
	stream := "{\"a\":1}\r\n" + "\n" + "{\"b\":2}\n"
	tr := &stdioTransport{reader: bufio.NewReader(strings.NewReader(stream))}

	ctx := context.Background()
	got, err := tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive 1: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("Receive 1 = %q; want the CRLF terminator stripped", got)
	}

	got, err = tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive 2: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("Receive 2 = %q; want an empty line to become {}", got)
	}

	got, err = tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive 3: %v", err)
	}
	if string(got) != `{"b":2}` {
		t.Fatalf("Receive 3 = %q", got)
	}

	if _, err = tr.Receive(ctx); err == nil {
		t.Fatal("Receive after the stream ended must return an error")
	}
}

func TestToolNameRoundTrip(t *testing.T) {
	cases := []struct {
		server, tool string
		want         string
	}{
		{"fs", "read_file", "mcp__fs__read_file"},
		{"fs", "mcp__other__already", "mcp__other__already"},
	}
	for _, tc := range cases {
		if got := ToolName(tc.server, tc.tool); got != tc.want {
			t.Fatalf("ToolName(%q,%q) = %q; want %q", tc.server, tc.tool, got, tc.want)
		}
	}

	parseCases := []struct {
		in                string
		wantSrv, wantTool string
	}{
		{"mcp__fs__read_file", "fs", "read_file"},
		{"mcp__fs__deep__tool", "fs", "deep__tool"},
		{"plain_tool", "", "plain_tool"},
		{"notmcp__fs__x", "", "notmcp__fs__x"},
	}
	for _, tc := range parseCases {
		srv, tool := ParseToolName(tc.in)
		if srv != tc.wantSrv || tool != tc.wantTool {
			t.Fatalf("ParseToolName(%q) = (%q,%q); want (%q,%q)", tc.in, srv, tool, tc.wantSrv, tc.wantTool)
		}
	}

	// ToolName and ParseToolName must be inverses for a simple pair.
	srv, tool := ParseToolName(ToolName("fs", "read_file"))
	if srv != "fs" || tool != "read_file" {
		t.Fatalf("round trip = (%q,%q)", srv, tool)
	}
}
