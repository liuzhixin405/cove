package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// autoServer is an in-memory MCP server: it answers initialize, tools/list and
// resources/list, can push notifications and can "crash" (EOF).
type autoServer struct {
	in       chan json.RawMessage
	dead     chan struct{}
	deadOnce sync.Once
	mu       sync.Mutex
	tools    []string
}

func newAutoServer(tools ...string) *autoServer {
	return &autoServer{in: make(chan json.RawMessage, 32), dead: make(chan struct{}), tools: tools}
}

func (s *autoServer) setTools(tools ...string) {
	s.mu.Lock()
	s.tools = tools
	s.mu.Unlock()
}

func (s *autoServer) Send(_ context.Context, msg any) error {
	data, _ := json.Marshal(msg)
	var m struct {
		ID     *int   `json:"id"`
		Method string `json:"method"`
	}
	_ = json.Unmarshal(data, &m)
	if m.ID == nil {
		return nil
	}
	var result string
	switch m.Method {
	case "initialize":
		result = `{"protocolVersion":"2024-11-05","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"auto","version":"1"}}`
	case "tools/list":
		s.mu.Lock()
		list := make([]map[string]any, 0, len(s.tools))
		for _, n := range s.tools {
			list = append(list, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		s.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"tools": list})
		result = string(b)
	case "resources/list":
		result = `{"resources":[]}`
	default:
		result = `{}`
	}
	select {
	case s.in <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *m.ID, result)):
	case <-s.dead:
	}
	return nil
}

func (s *autoServer) Receive(ctx context.Context) (json.RawMessage, error) {
	select {
	case m := <-s.in:
		return m, nil
	case <-s.dead:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *autoServer) crash()       { s.deadOnce.Do(func() { close(s.dead) }) }
func (s *autoServer) Close() error { s.crash(); return nil }

func toolNames(p *Pool) []string {
	var out []string
	for _, r := range p.AllTools() {
		out = append(out, r.Tool.Name)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The first disconnect of a server is followed by one automatic reconnect
// (after reconnectDelay); a second disconnect is left to /mcp connect.
func TestPoolReconnectsOnceAfterDisconnect(t *testing.T) {
	old := reconnectDelay
	reconnectDelay = 10 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = old })

	var dials atomic.Int32
	var mu sync.Mutex
	var servers []*autoServer
	p := NewPool()
	p.dial = func(string, ServerConfig) (Transport, error) {
		dials.Add(1)
		s := newAutoServer("t")
		mu.Lock()
		servers = append(servers, s)
		mu.Unlock()
		return s, nil
	}
	if err := p.Connect(context.Background(), "srv", ServerConfig{}); err != nil {
		t.Fatal(err)
	}
	defer p.DisconnectAll()

	mu.Lock()
	servers[0].crash()
	mu.Unlock()
	waitFor(t, "reconnect", func() bool {
		s := p.AllServers()
		return dials.Load() == 2 && len(s) == 1 && s[0].Connected
	})

	mu.Lock()
	servers[1].crash()
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if n := dials.Load(); n != 2 {
		t.Fatalf("dialled %d times; only one automatic reconnect is allowed", n)
	}
}

// A deliberate Disconnect is not a crash and must not trigger a reconnect.
func TestPoolNoReconnectAfterDeliberateDisconnect(t *testing.T) {
	old := reconnectDelay
	reconnectDelay = 10 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = old })
	var dials atomic.Int32
	p := NewPool()
	p.dial = func(string, ServerConfig) (Transport, error) {
		dials.Add(1)
		return newAutoServer("t"), nil
	}
	if err := p.Connect(context.Background(), "srv", ServerConfig{}); err != nil {
		t.Fatal(err)
	}
	p.Disconnect("srv")
	time.Sleep(100 * time.Millisecond)
	if n := dials.Load(); n != 1 {
		t.Fatalf("dialled %d times after Disconnect", n)
	}
}

// notifications/tools/list_changed refreshes the pool's tool list.
func TestPoolRefreshesToolsOnListChanged(t *testing.T) {
	srv := newAutoServer("a")
	p := NewPool()
	p.dial = func(string, ServerConfig) (Transport, error) { return srv, nil }
	if err := p.Connect(context.Background(), "srv", ServerConfig{}); err != nil {
		t.Fatal(err)
	}
	defer p.DisconnectAll()
	if got := toolNames(p); len(got) != 1 || got[0] != "a" {
		t.Fatalf("tools = %v", got)
	}
	srv.setTools("a", "b")
	srv.in <- json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	waitFor(t, "tool refresh", func() bool { return len(toolNames(p)) == 2 })
}

// Version changes whenever the pool's tool list does, so the engine can
// rebuild its cached tool definitions.
func TestPoolVersionTracksToolChanges(t *testing.T) {
	srv := newAutoServer("a")
	p := NewPool()
	p.dial = func(string, ServerConfig) (Transport, error) { return srv, nil }
	v0 := p.Version()
	if err := p.Connect(context.Background(), "srv", ServerConfig{}); err != nil {
		t.Fatal(err)
	}
	v1 := p.Version()
	if v1 == v0 {
		t.Fatal("Connect did not bump Version")
	}
	srv.setTools("a", "b")
	srv.in <- json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	waitFor(t, "version bump", func() bool { return p.Version() != v1 })
	v2 := p.Version()
	p.Disconnect("srv")
	if p.Version() == v2 {
		t.Fatal("Disconnect did not bump Version")
	}
}
