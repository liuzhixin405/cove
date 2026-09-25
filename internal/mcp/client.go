package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/textutil"
)

type Transport interface {
	Send(ctx context.Context, msg any) error
	Receive(ctx context.Context) (json.RawMessage, error)
	Close() error
}

type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	reader *bufio.Reader

	// sendSem serialises writes (a one-slot semaphore rather than a mutex so a
	// waiting Send can give up on its context). broken is set, under sendSem,
	// once a write was abandoned half-way: the stream is then corrupt.
	sendSem chan struct{}
	broken  error

	// closeOnce guards the shutdown path: Close is reachable from Pool.Connect's
	// error handling and from Client.Close, and cmd.Wait must run exactly once.
	closeOnce sync.Once
	waitErr   error

	// tree lets Close kill the server's descendants, not just the direct child.
	tree procTree
}

func NewStdioTransport(command string, args []string, env map[string]string) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	// The child's stderr is PIPED, not inherited.
	//
	// cmd.Stderr = os.Stderr gave a third-party MCP server the real terminal.
	// That is the one output path no Go-side change elsewhere can intercept:
	// the server is free to emit progress meters, carriage returns and cursor
	// moves, which land past whatever the UI believes is on screen and leave a
	// pinned input box drifting. It is also a correctness problem on its own —
	// a chatty server could scribble over the conversation at any moment.
	//
	// Draining it is mandatory: an unread pipe fills its buffer and then the
	// child blocks forever on its next write.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	setupProcTree(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	t := &stdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		reader:  bufio.NewReader(stdout),
		sendSem: make(chan struct{}, 1),
	}
	t.tree.attach(cmd)

	go drainServerStderr(command, stderr)
	return t, nil
}

// maxServerStderrLine bounds one logged line from a misbehaving server.
const maxServerStderrLine = 2000

// drainServerStderr consumes the child's stderr and logs it at debug level.
//
// It exits when the pipe closes, which happens when the process exits — so it
// cannot outlive the server it belongs to.
//
// Lines of any length are consumed. This used a Scanner capped at 256KB per
// line, which gave up at the first longer line and closed the pipe; every
// later stderr write of the server then failed with EPIPE, which crashes a
// Node server outright. Only the start of a long line is kept for the log.
func drainServerStderr(name string, r io.ReadCloser) {
	defer func() { _ = r.Close() }()
	br := bufio.NewReaderSize(r, 8*1024)
	var line []byte
	for {
		frag, isPrefix, err := br.ReadLine()
		// Keep a margin over the logged size: sanitizing removes escapes.
		if room := 4*maxServerStderrLine - len(line); room > 0 {
			if len(frag) > room {
				frag = frag[:room]
			}
			line = append(line, frag...)
		}
		if isPrefix && err == nil {
			continue
		}
		logServerLine(name, string(line))
		line = line[:0]
		if err != nil {
			return
		}
	}
}

func logServerLine(name, line string) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return
	}
	// Strip control sequences before logging: the whole point of piping this
	// is that the server's escapes must never reach a terminal, and the log's
	// writer may well BE the terminal.
	log.Debugf("mcp %s: %s", name, sanitizeServerLine(line))
}

// sanitizeServerLine removes every escape sequence and bare control byte from
// one line of third-party output, and clips it on a rune boundary.
func sanitizeServerLine(s string) string {
	s = ansi.Strip(s)
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return textutil.ClipBytes(s, maxServerStderrLine, "…")
}

// Send writes one message, honouring ctx.
//
// A server that stops draining its stdin (deadlocked, or busy in synchronous
// work) makes the write block once the pipe buffer is full. Send used to block
// there forever, ignoring its context and holding the lock, so the tool call
// could not be interrupted and every later call to the server queued behind
// it. Now the write runs aside; if ctx ends first the transport is shut down -
// a half-written message has corrupted the stream anyway - which also unblocks
// the abandoned write.
func (t *stdioTransport) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	select {
	case t.sendSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if t.broken != nil {
		<-t.sendSem
		return t.broken
	}

	done := make(chan error, 1)
	go func() {
		_, err := t.stdin.Write(data)
		done <- err
	}()
	select {
	case err := <-done:
		<-t.sendSem
		return err
	case <-ctx.Done():
		// Both may be ready at once; a write that did complete leaves the
		// stream intact and must not cost the connection.
		select {
		case err := <-done:
			<-t.sendSem
			return err
		default:
		}
		t.broken = fmt.Errorf("mcp stdio: connection abandoned after an interrupted write")
		<-t.sendSem
		go func() { _ = t.Close() }()
		return ctx.Err()
	}
}

func (t *stdioTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	line, err := t.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	// ReadBytes keeps the delimiter, so the '\n' has to go before looking for a
	// CR. Without this the last byte was always '\n', stripCR never matched,
	// and both the CRLF handling and the empty-line case below were dead code:
	// every message was handed on with its line terminator still attached and a
	// blank line surfaced as "\n" instead of "{}".
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	line = stripCR(line)
	if len(line) == 0 {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(line), nil
}

// stdioCloseGrace is how long a server gets to exit on its own after its stdin
// is closed, before it is killed.
const stdioCloseGrace = 2 * time.Second

// Close shuts the child server down and reaps it.
//
// Closing stdin is the protocol's own shutdown signal, so a well-behaved server
// exits by itself; only a server that ignores it gets killed. Either way the
// process must be Wait()ed, otherwise its entry stays in the OS process table
// as a zombie — and since every disconnect/reconnect cycle spawns a fresh
// server, those accumulated for the lifetime of the session.
func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		_ = t.stdin.Close()

		if t.cmd.Process == nil {
			return
		}

		done := make(chan error, 1)
		go func() { done <- t.cmd.Wait() }()

		select {
		case t.waitErr = <-done:
		case <-time.After(stdioCloseGrace):
			t.tree.kill(t.cmd)
			// Still reap: Kill only delivers the signal, Wait releases the
			// process entry and the pipe goroutines.
			t.waitErr = <-done
		}
		t.tree.release()
	})
	return t.waitErr
}

func stripCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

type Client struct {
	transport  Transport
	serverCaps ServerCaps
	serverInfo Implementation
	reqID      int
	mu         sync.Mutex
	pending    map[int]chan *Response
	notifyCh   chan *Notification
	closed     bool
	// dead is set when receiveLoop exits (transport EOF, server crash). Without
	// it a Call after the server died registered a pending entry nobody would
	// ever resolve, sent the request (HTTP transports accept it happily) and
	// then waited for the caller's context - for a tool call, until the user
	// interrupted the turn.
	dead   bool
	stopCh chan struct{} // closed by Close() to signal receiveLoop to stop
	// done is closed when receiveLoop exits (see Done).
	done     chan struct{}
	doneOnce sync.Once
}

func NewClient(transport Transport) *Client {
	return &Client{
		transport: transport,
		pending:   make(map[int]chan *Response),
		notifyCh:  make(chan *Notification, 64),
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// handshakeTimeout bounds the initialize round-trip. A server that accepts the
// connection but never answers initialize would otherwise block Connect
// forever, and with it every caller waiting on the pool.
const handshakeTimeout = 20 * time.Second

func (c *Client) Connect(ctx context.Context) error {
	go c.receiveLoop()

	// Apply a handshake deadline unless the caller already set a shorter one.
	handshakeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	ctx = handshakeCtx

	params := InitializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo:      Implementation{Name: "cove", Version: "0.2.0"},
		Capabilities:    ClientCaps{},
	}

	var result InitializeResult
	if err := c.Call(ctx, "initialize", params, &result); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.serverCaps = result.Capabilities
	c.serverInfo = result.ServerInfo

	_ = c.SendNotification(ctx, "notifications/initialized", nil)
	return nil
}

func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		// Close() has already torn down receiveLoop, so nothing will ever
		// resolve a pending entry registered from here. Without this check the
		// request was registered and sent anyway (many transports accept a
		// write after Close without erroring) and the caller then sat in the
		// select below until its context expired — forever, for a context with
		// no deadline.
		return fmt.Errorf("mcp: connection closed")
	}
	if c.dead {
		c.mu.Unlock()
		return fmt.Errorf("mcp: connection lost (server exited or closed the stream); reconnect with /mcp connect")
	}
	c.reqID++
	id := c.reqID
	ch := make(chan *Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	var p json.RawMessage
	if params != nil {
		data, _ := json.Marshal(params)
		p = data
	}

	req := Request{
		JSONRPC: JSONRPC{Jsonrpc: "2.0"},
		ID:      id,
		Method:  method,
		Params:  p,
	}

	if err := c.transport.Send(ctx, req); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	select {
	case resp, ok := <-ch:
		// Close() closes pending channels on shutdown; a closed channel yields a
		// nil *Response. Guard against it instead of dereferencing resp.Error.
		if !ok || resp == nil {
			return fmt.Errorf("mcp: connection closed before response")
		}
		if resp.Error != nil {
			return fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if result != nil && resp.Result != nil {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		// Tell the server to stop: without this an interrupted tool call kept
		// running on the server (a long search, a build) to completion. The
		// spec forbids cancelling initialize. Sent aside, with its own short
		// deadline, since ctx is already done.
		if method != "initialize" {
			go func() {
				nctx, cancel := context.WithTimeout(context.Background(), serverReplyTimeout)
				defer cancel()
				_ = c.SendNotification(nctx, "notifications/cancelled",
					map[string]any{"requestId": id, "reason": ctx.Err().Error()})
			}()
		}
		return ctx.Err()
	}
}

func (c *Client) SendNotification(ctx context.Context, method string, params any) error {
	var p json.RawMessage
	if params != nil {
		data, _ := json.Marshal(params)
		p = data
	}
	notif := Notification{
		JSONRPC: JSONRPC{Jsonrpc: "2.0"},
		Method:  method,
		Params:  p,
	}
	return c.transport.Send(ctx, notif)
}

func (c *Client) Notifications() <-chan *Notification {
	return c.notifyCh
}

func (c *Client) ServerInfo() Implementation { return c.serverInfo }

// Alive reports whether the connection can still carry requests: the client has
// not been closed and its receive loop is still running. A server that crashed
// or dropped its stream leaves the client open but dead.
func (c *Client) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !c.dead
}

func (c *Client) Close() error {
	// Signal receiveLoop to stop (non-blocking: channel may already be closed).
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}

	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	return c.transport.Close()
}

// Done is closed once the connection's receive loop has stopped: the server
// exited or dropped the stream, or Close was called (see ClosedByUser).
func (c *Client) Done() <-chan struct{} { return c.done }

// ClosedByUser reports whether Close was called, as opposed to the connection
// dying on its own.
func (c *Client) ClosedByUser() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

func (c *Client) receiveLoop() {
	defer c.doneOnce.Do(func() { close(c.done) })
	defer func() {
		c.mu.Lock()
		c.dead = true
		for _, ch := range c.pending {
			// Non-blocking: each pending channel is buffered for exactly one
			// response. A blocking send here deadlocked the whole client — if a
			// caller abandoned its request (context cancelled) after its
			// response had already been buffered, this broadcast blocked on the
			// full channel while holding c.mu, and nothing would ever drain it,
			// so every later Call and Close hung forever.
			select {
			case ch <- &Response{Error: &Error{Code: -32000, Message: "transport closed or connection lost"}}:
			default:
			}
		}
		c.mu.Unlock()
	}()

	// Wrap the blocking Receive with a cancellable select so receiveLoop
	// can exit promptly when Close() closes stopCh, even if the transport
	// is blocked on ReadBytes. The spawned goroutine is temporary: when
	// the transport pipe closes (via Close()->transport.Close()->
	// process kill), ReadBytes returns an error and the goroutine exits.
	type readResult struct {
		raw json.RawMessage
		err error
	}

	for {
		ch := make(chan readResult, 1)
		go func() {
			raw, err := c.transport.Receive(context.Background())
			ch <- readResult{raw, err}
		}()

		select {
		case <-c.stopCh:
			return
		case res := <-ch:
			if res.err != nil {
				return
			}
			if err := c.handleRaw(res.raw); err != nil {
				return
			}
		}
	}
}

// handleRaw processes a single JSON-RPC message received from the transport.
// Extracted from receiveLoop to keep the cancellation wrapper clean.
func (c *Client) handleRaw(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}

	var base struct {
		JSONRPC
		// Raw so we can tell "no id" (notification) from "id: 0", and so a
		// server request's id - which may be a string - can be echoed back
		// verbatim. It used to be *int: a string id failed the whole decode and
		// the message was dropped.
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *Error          `json:"error,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
	}

	if err := json.Unmarshal(raw, &base); err != nil {
		return nil
	}
	hasID := len(base.ID) > 0 && string(base.ID) != "null"

	// Notification: a method with no id.
	if base.Method != "" && !hasID {
		notif := &Notification{
			JSONRPC: JSONRPC{Jsonrpc: base.Jsonrpc},
			Method:  base.Method,
			Params:  base.Params,
		}
		select {
		case c.notifyCh <- notif:
		default:
		}
		return nil
	}

	// Server-initiated request (method + id). It must never be misrouted into
	// the pending-response table, and it must be answered: silence used to
	// leave servers waiting forever, and ones that ping as a keepalive dropped
	// the connection. Only ping is implemented (sampling/roots are not
	// advertised). The reply goes out on its own goroutine so a slow Send (an
	// HTTP POST) cannot stall the receive loop.
	if base.Method != "" {
		go c.answerServerRequest(base.ID, base.Method)
		return nil
	}

	// Otherwise it's a response to one of our requests; it must carry an id.
	if !hasID {
		return nil
	}
	id, ok := responseID(base.ID)
	if !ok {
		return nil
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("client closed")
	}
	ch, ok := c.pending[id]
	if ok {
		// Non-blocking: the channel is buffered for the single response this
		// request expects, and a second response for the same id is a protocol
		// violation. A blocking send here ran under c.mu, so a server that
		// answered the same id twice (or a caller that gave up after its
		// response was buffered) pinned the mutex permanently and froze every
		// other Call and Close on this client.
		select {
		case ch <- &Response{
			JSONRPC: JSONRPC{Jsonrpc: base.Jsonrpc},
			ID:      id,
			Result:  base.Result,
			Error:   base.Error,
		}:
		default:
		}
	}
	c.mu.Unlock()
	return nil
}

// responseID decodes the id of a response to one of our requests. We only ever
// send integer ids, but a server that echoes them back as strings is tolerated.
func responseID(raw json.RawMessage) (int, bool) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.Atoi(s); err == nil {
			return n, true
		}
	}
	return 0, false
}

// rpcReply is a response to a server-initiated request. Its id is raw so the
// server's own id (string or number) goes back exactly as it was sent.
type rpcReply struct {
	JSONRPC
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// serverReplyTimeout bounds the Send of a reply to a server request.
const serverReplyTimeout = 10 * time.Second

func (c *Client) answerServerRequest(id json.RawMessage, method string) {
	reply := rpcReply{JSONRPC: JSONRPC{Jsonrpc: "2.0"}, ID: id}
	if method == "ping" {
		reply.Result = struct{}{}
	} else {
		reply.Error = &Error{Code: -32601, Message: "Method not found: " + method}
	}
	ctx, cancel := context.WithTimeout(context.Background(), serverReplyTimeout)
	defer cancel()
	_ = c.transport.Send(ctx, reply)
}

// maxListPages bounds cursor pagination, so a server that keeps handing out
// fresh cursors cannot keep a listing going forever.
const maxListPages = 100

// cursorParams is the params object of a paginated list request.
func cursorParams(cursor string) any {
	if cursor == "" {
		return nil
	}
	return map[string]string{"cursor": cursor}
}

// ListTools returns every tool the server offers, following nextCursor.
// Reading only the first page silently hid the rest of a large server's tools.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if c.serverCaps.Tools == nil {
		return nil, fmt.Errorf("server does not support tools")
	}
	var all []Tool
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		var result ListToolsResult
		if err := c.Call(ctx, "tools/list", cursorParams(cursor), &result); err != nil {
			return nil, err
		}
		all = append(all, result.Tools...)
		cursor = result.NextCursor
		if cursor == "" || seen[cursor] {
			break
		}
		seen[cursor] = true
	}
	return all, nil
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	params := CallToolParams{Name: name, Arguments: args}
	var result CallToolResult
	if err := c.Call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	if c.serverCaps.Resources == nil {
		return nil, fmt.Errorf("server does not support resources")
	}
	var all []Resource
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		var result ListResourcesResult
		if err := c.Call(ctx, "resources/list", cursorParams(cursor), &result); err != nil {
			return nil, err
		}
		all = append(all, result.Resources...)
		cursor = result.NextCursor
		if cursor == "" || seen[cursor] {
			break
		}
		seen[cursor] = true
	}
	return all, nil
}

func (c *Client) ReadResource(ctx context.Context, uri string) (*ReadResourceResult, error) {
	params := ReadResourceParams{URI: uri}
	var result ReadResourceResult
	if err := c.Call(ctx, "resources/read", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func ToolName(server, tool string) string {
	if !strings.Contains(tool, "__") {
		return fmt.Sprintf("mcp__%s__%s", server, tool)
	}
	return tool
}

func ParseToolName(mcpToolName string) (server, tool string) {
	parts := strings.SplitN(mcpToolName, "__", 3)
	if len(parts) == 3 && parts[0] == "mcp" {
		return parts[1], parts[2]
	}
	return "", mcpToolName
}
