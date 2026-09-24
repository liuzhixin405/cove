package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove/internal/log"
)

type ManagedServer struct {
	Name      string
	Config    ServerConfig
	Client    *Client
	Transport Transport
	Tools     []Tool
	Resources []Resource
	Connected bool
	Err       string
}

type Pool struct {
	servers map[string]*ManagedServer
	mu      sync.RWMutex
	dial    func(name string, cfg ServerConfig) (Transport, error)
}

func NewPool() *Pool {
	return &Pool{servers: make(map[string]*ManagedServer)}
}

// Connect brings up one server and registers it in the pool.
//
// The handshake, ListTools and ListResources round-trips all happen WITHOUT the
// pool lock held. Holding p.mu across them meant one unresponsive server (a
// hung initialize, a slow tool listing) blocked every other pool operation —
// including reads from unrelated servers — for as long as it took to time out.
// The lock is taken only for the two short critical sections: claiming the name
// and publishing the finished server.
func (p *Pool) Connect(ctx context.Context, name string, cfg ServerConfig) error {
	p.mu.Lock()
	var stale *ManagedServer
	if existing, ok := p.servers[name]; ok {
		// A server whose connection died (child crashed, stream dropped) is
		// still in the map with Connected set; treating it as live made
		// "/mcp connect" report success while doing nothing.
		if existing.live() {
			p.mu.Unlock()
			return nil
		}
		stale = existing
		delete(p.servers, name)
	}
	p.mu.Unlock()
	// Evicting the dead server is unregistration (under the lock) plus a
	// shutdown (outside it). Close reaps the child process and waits up to
	// stdioCloseGrace for it to exit; running that under p.mu froze every other
	// pool operation for seconds, which is exactly what the rest of this file
	// takes care to avoid.
	if stale != nil {
		stale.Close()
	}

	dial := p.dial
	if dial == nil {
		dial = newTransport
	}
	transport, err := dial(name, cfg)
	if err != nil {
		return p.recordFailure(name, cfg, fmt.Errorf("transport for %s: %w", name, err))
	}

	client := NewClient(transport)
	if err := client.Connect(ctx); err != nil {
		_ = transport.Close()
		return p.recordFailure(name, cfg, fmt.Errorf("connect %s: %w", name, err))
	}

	ms := &ManagedServer{
		Name:      name,
		Config:    cfg,
		Client:    client,
		Transport: transport,
		Connected: true,
	}

	if tools, err := client.ListTools(ctx); err == nil {
		ms.Tools = tools
	}

	if resources, err := client.ListResources(ctx); err == nil {
		ms.Resources = resources
	}

	p.mu.Lock()
	// A concurrent Connect for the same name may have won the race while this
	// one was handshaking; keep the existing live server and discard ours.
	if existing, ok := p.servers[name]; ok && existing.live() {
		p.mu.Unlock()
		ms.Close()
		return nil
	}
	p.servers[name] = ms
	p.mu.Unlock()
	return nil
}

// recordFailure keeps a server that failed to connect in the pool, marked not
// connected and carrying its error, and returns err. Dropping it made a broken
// server simply vanish: the error only reached the debug log, and "/mcp list"
// claimed no servers were configured. A live entry is never overwritten.
func (p *Pool) recordFailure(name string, cfg ServerConfig, err error) error {
	p.mu.Lock()
	if existing, ok := p.servers[name]; !ok || !existing.live() {
		p.servers[name] = &ManagedServer{Name: name, Config: cfg, Err: err.Error()}
	}
	p.mu.Unlock()
	return err
}

// newTransport picks the transport for a server config. The type is matched
// loosely because every ecosystem spells it differently (the docs say
// "streamable_http", other MCP clients say "http", the SDKs "streamable-http"), and
// an unknown type is an error: it used to fall through to stdio and fail with
// "command is required", which pointed at the wrong field entirely.
func newTransport(name string, cfg ServerConfig) (Transport, error) {
	typ := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(cfg.Type))
	switch typ {
	case "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("sse type requires 'url' in config")
		}
		return NewSSETransport(cfg.URL)
	case "http", "streamablehttp":
		if cfg.URL == "" {
			return nil, fmt.Errorf("%s type requires 'url' in config", cfg.Type)
		}
		return NewStreamableHTTPTransport(cfg.URL)
	case "", "stdio":
		return NewSTDIOTransport(name, cfg)
	default:
		return nil, fmt.Errorf("unknown transport type %q (want stdio, sse or http)", cfg.Type)
	}
}

func NewSTDIOTransport(name string, cfg ServerConfig) (Transport, error) {
	if err := validateSTDIOCommand(cfg.Command); err != nil {
		return nil, fmt.Errorf("server %s: %w", name, err)
	}
	args := cfg.Args
	env := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		env[k] = v
	}
	return NewStdioTransport(cfg.Command, args, env)
}

func validateSTDIOCommand(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("command is required")
	}
	if strings.ContainsAny(command, ";&|`$<>\r\n") {
		return fmt.Errorf("command contains shell control characters")
	}

	base := strings.ToLower(filepath.Base(command))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	shells := map[string]bool{
		"sh": true, "bash": true, "zsh": true, "ksh": true, "fish": true,
		"cmd": true, "powershell": true, "pwsh": true,
	}
	if shells[base] {
		return fmt.Errorf("shell wrappers are not allowed for MCP stdio servers: %s", command)
	}
	return nil
}

// Disconnect removes a server from the pool and shuts it down.
//
// The server is unregistered under the lock but closed after releasing it:
// Close reaps the child process and waits up to stdioCloseGrace for it to
// exit, and holding the pool lock across that would block every other pool
// operation for seconds.
func (p *Pool) Disconnect(name string) {
	p.mu.Lock()
	s, ok := p.servers[name]
	if ok {
		delete(p.servers, name)
	}
	p.mu.Unlock()
	if ok {
		s.Close()
	}
}

func (p *Pool) DisconnectAll() {
	p.mu.Lock()
	doomed := make([]*ManagedServer, 0, len(p.servers))
	for name, s := range p.servers {
		doomed = append(doomed, s)
		delete(p.servers, name)
	}
	p.mu.Unlock()

	// Shut the servers down concurrently and outside the lock. Serially under
	// the lock, N unresponsive servers would each burn the full close grace
	// period in turn — N × stdioCloseGrace of a frozen pool on shutdown.
	var wg sync.WaitGroup
	for _, s := range doomed {
		wg.Add(1)
		go func(ms *ManagedServer) {
			defer wg.Done()
			ms.Close()
		}(s)
	}
	wg.Wait()
}

// AllServers returns a snapshot of the pool's servers.
//
// The returned values are copies, not the pool's live *ManagedServer records.
// Handing out the live pointers let callers read Connected/Tools/Resources
// after releasing the lock, racing Close() and Connect() which write those same
// fields. Callers only ever read the metadata, so a snapshot costs nothing.
//
// Every listing is sorted by server name. The mcp tool's description is rebuilt
// from AllTools on each request, and map iteration order made it differ from
// turn to turn - a changed tool definition in the prompt prefix, which throws
// away the provider's prompt cache (DeepSeek and Anthropic both cache by
// prefix) on every single request.
func (p *Pool) AllServers() []*ManagedServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*ManagedServer, 0, len(p.servers))
	for _, name := range p.sortedNames() {
		result = append(result, p.servers[name].snapshot())
	}
	return result
}

// sortedNames returns the pool's server names in order. Callers hold p.mu.
func (p *Pool) sortedNames() []string {
	names := make([]string, 0, len(p.servers))
	for name := range p.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// snapshot returns a copy safe to read outside the pool lock. Client and
// Transport are carried over as-is (they have their own internal locking); the
// mutable metadata fields are copied.
func (ms *ManagedServer) snapshot() *ManagedServer {
	cp := &ManagedServer{
		Name:      ms.Name,
		Config:    ms.Config,
		Client:    ms.Client,
		Transport: ms.Transport,
		Connected: ms.live(),
		Err:       ms.Err,
	}
	if len(ms.Tools) > 0 {
		cp.Tools = append([]Tool(nil), ms.Tools...)
	}
	if len(ms.Resources) > 0 {
		cp.Resources = append([]Resource(nil), ms.Resources...)
	}
	return cp
}

func (p *Pool) AllTools() []ToolRef {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var refs []ToolRef
	for _, name := range p.sortedNames() {
		for _, t := range p.servers[name].Tools {
			refs = append(refs, ToolRef{Server: name, Tool: t})
		}
	}
	return refs
}

// client returns the connection for serverName, or an error that says why
// there is none (never configured, or its connect failed and with what).
func (p *Pool) client(serverName string) (*Client, error) {
	p.mu.RLock()
	s, ok := p.servers[serverName]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mcp server %s not connected", serverName)
	}
	if s.Client == nil {
		return nil, fmt.Errorf("mcp server %s not connected: %s", serverName, s.Err)
	}
	return s.Client, nil
}

func (p *Pool) CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (*CallToolResult, error) {
	c, err := p.client(serverName)
	if err != nil {
		return nil, err
	}
	return c.CallTool(ctx, toolName, args)
}

func (p *Pool) AllResources() []ResourceRef {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var refs []ResourceRef
	for _, name := range p.sortedNames() {
		for _, r := range p.servers[name].Resources {
			refs = append(refs, ResourceRef{Server: name, Resource: r})
		}
	}
	return refs
}

func (p *Pool) ReadResource(ctx context.Context, serverName, uri string) (*ReadResourceResult, error) {
	c, err := p.client(serverName)
	if err != nil {
		return nil, err
	}
	return c.ReadResource(ctx, uri)
}

type ToolRef struct {
	Server string
	Tool   Tool
}

type ResourceRef struct {
	Server   string
	Resource Resource
}

// live reports whether the server is connected AND its connection still works.
func (ms *ManagedServer) live() bool {
	return ms.Connected && ms.Client != nil && ms.Client.Alive()
}

func (ms *ManagedServer) Close() {
	ms.Connected = false
	if ms.Client != nil {
		_ = ms.Client.Close()
	}
	// Note: transport is already closed by Client.Close(), no double-close
}

// LoadFromConfig connects to all servers defined in the config map.
//
// The servers are dialled concurrently. Startup gives MCP a single shared
// deadline, and connecting one after another meant a single server that never
// answered initialize used up the whole budget, so every server after it
// failed with an expired context. Failures are recorded in the pool (see
// recordFailure) so /mcp list can show them.
func (p *Pool) LoadFromConfig(ctx context.Context, servers map[string]ServerConfig) {
	var wg sync.WaitGroup
	for name, cfg := range servers {
		wg.Add(1)
		go func(name string, cfg ServerConfig) {
			defer wg.Done()
			if err := p.Connect(ctx, name, cfg); err != nil {
				logF("MCP: %s: %v", name, err)
			}
		}(name, cfg)
	}
	wg.Wait()
}

// logF emits diagnostic messages only when the global log level is Debug,
// so MCP connection problems stay hidden in normal (release) runs.
func logF(format string, args ...any) {
	log.Debugf(format, args...)
}
