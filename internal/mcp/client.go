package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	mu     sync.Mutex
	reader *bufio.Reader

	// closeOnce guards the shutdown path: Close is reachable from Pool.Connect's
	// error handling and from Client.Close, and cmd.Wait must run exactly once.
	closeOnce sync.Once
	waitErr   error
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

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	go drainServerStderr(command, stderr)

	return &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		reader: bufio.NewReader(stdout),
	}, nil
}

// maxServerStderrLine bounds one logged line from a misbehaving server.
const maxServerStderrLine = 2000

// drainServerStderr consumes the child's stderr and logs it at debug level.
//
// It exits when the pipe closes, which happens when the process exits — so it
// cannot outlive the server it belongs to.
func drainServerStderr(name string, r io.ReadCloser) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8*1024), 256*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Strip control sequences before logging: the whole point of piping
		// this is that the server's escapes must never reach a terminal, and
		// the log's writer may well BE the terminal.
		log.Debugf("mcp %s: %s", name, sanitizeServerLine(line))
	}
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

func (t *stdioTransport) Send(ctx context.Context, msg any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = t.stdin.Write(data)
	return err
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
		t.stdin.Close()

		if t.cmd.Process == nil {
			return
		}

		done := make(chan error, 1)
		go func() { done <- t.cmd.Wait() }()

		select {
		case t.waitErr = <-done:
		case <-time.After(stdioCloseGrace):
			t.cmd.Process.Kill()
			// Still reap: Kill only delivers the signal, Wait releases the
			// process entry and the pipe goroutines.
			t.waitErr = <-done
		}
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
	stopCh     chan struct{} // closed by Close() to signal receiveLoop to stop
}

func NewClient(transport Transport) *Client {
	return &Client{
		transport: transport,
		pending:   make(map[int]chan *Response),
		notifyCh:  make(chan *Notification, 64),
		stopCh:    make(chan struct{}),
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

	c.SendNotification(ctx, "notifications/initialized", nil)
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

func (c *Client) receiveLoop() {
	defer func() {
		c.mu.Lock()
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
		// Pointer so we can tell "no id" (notification) from "id: 0". With a
		// plain int+omitempty, a response with id 0 and a notification were
		// indistinguishable, so id-0 responses were dropped and id-0 server
		// requests were misrouted as notifications.
		ID     *int            `json:"id"`
		Method string          `json:"method,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *Error          `json:"error,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
	}

	if err := json.Unmarshal(raw, &base); err != nil {
		return nil
	}

	// Notification: a method with no id.
	if base.Method != "" && base.ID == nil {
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

	// Server-initiated request (method + id): this client doesn't implement
	// server-to-client requests (sampling/roots). Ignore rather than misroute it
	// into the pending-response table.
	if base.Method != "" {
		return nil
	}

	// Otherwise it's a response to one of our requests; it must carry an id.
	if base.ID == nil {
		return nil
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("client closed")
	}
	ch, ok := c.pending[*base.ID]
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
			ID:      *base.ID,
			Result:  base.Result,
			Error:   base.Error,
		}:
		default:
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if c.serverCaps.Tools == nil {
		return nil, fmt.Errorf("server does not support tools")
	}
	var result ListToolsResult
	if err := c.Call(ctx, "tools/list", nil, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
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
	var result ListResourcesResult
	if err := c.Call(ctx, "resources/list", nil, &result); err != nil {
		return nil, err
	}
	return result.Resources, nil
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
