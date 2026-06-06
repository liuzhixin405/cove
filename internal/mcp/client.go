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
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	return &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		reader: bufio.NewReader(stdout),
	}, nil
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
	line = stripCR(line)
	if len(line) == 0 {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(line), nil
}

func (t *stdioTransport) Close() error {
	t.stdin.Close()
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
	return nil
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
}

func NewClient(transport Transport) *Client {
	return &Client{
		transport: transport,
		pending:   make(map[int]chan *Response),
		notifyCh:  make(chan *Notification, 64),
	}
}

func (c *Client) Connect(ctx context.Context) error {
	go c.receiveLoop()

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
	case resp := <-ch:
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

func (c *Client) ServerCapabilities() ServerCaps { return c.serverCaps }
func (c *Client) ServerInfo() Implementation     { return c.serverInfo }

func (c *Client) Close() error {
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
			ch <- &Response{Error: &Error{Code: -32000, Message: "transport closed or connection lost"}}
		}
		c.mu.Unlock()
	}()

	for {
		raw, err := c.transport.Receive(context.Background())
		if err != nil {
			return
		}

		if len(raw) == 0 {
			continue
		}

		var base struct {
			JSONRPC
			ID     int             `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *Error          `json:"error,omitempty"`
			Params json.RawMessage `json:"params,omitempty"`
		}

		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}

		if base.Method != "" && base.ID == 0 {
			notif := &Notification{
				JSONRPC: JSONRPC{Jsonrpc: base.Jsonrpc},
				Method:  base.Method,
				Params:  base.Params,
			}
			select {
			case c.notifyCh <- notif:
			default:
			}
			continue
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		ch, ok := c.pending[base.ID]
		if ok {
			ch <- &Response{
				JSONRPC: JSONRPC{Jsonrpc: base.Jsonrpc},
				ID:      base.ID,
				Result:  base.Result,
				Error:   base.Error,
			}
		}
		c.mu.Unlock()
	}
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

func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	if c.serverCaps.Prompts == nil {
		return nil, fmt.Errorf("server does not support prompts")
	}
	var result ListPromptsResult
	if err := c.Call(ctx, "prompts/list", nil, &result); err != nil {
		return nil, err
	}
	return result.Prompts, nil
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
