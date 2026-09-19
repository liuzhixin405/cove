package mcp

import (
	"context"
	"fmt"
	"path/filepath"
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
}

type Pool struct {
	servers map[string]*ManagedServer
	mu      sync.RWMutex
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
		if existing.Connected {
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

	var transport Transport
	var err error

	switch strings.ToLower(cfg.Type) {
	case "sse":
		if cfg.URL != "" {
			transport, err = NewSSETransport(cfg.URL)
		} else {
			err = fmt.Errorf("sse type requires 'url' in config")
		}
	case "http", "streamablehttp":
		if cfg.URL != "" {
			transport, err = NewStreamableHTTPTransport(cfg.URL)
		} else {
			err = fmt.Errorf("%s type requires 'url' in config", cfg.Type)
		}
	default:
		transport, err = NewSTDIOTransport(name, cfg)
	}

	if err != nil {
		return fmt.Errorf("transport for %s: %w", name, err)
	}

	client := NewClient(transport)
	if err := client.Connect(ctx); err != nil {
		_ = transport.Close()
		return fmt.Errorf("connect %s: %w", name, err)
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
	if existing, ok := p.servers[name]; ok && existing.Connected {
		p.mu.Unlock()
		ms.Close()
		return nil
	}
	p.servers[name] = ms
	p.mu.Unlock()
	return nil
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
func (p *Pool) AllServers() []*ManagedServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*ManagedServer, 0, len(p.servers))
	for _, s := range p.servers {
		result = append(result, s.snapshot())
	}
	return result
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
		Connected: ms.Connected,
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
	for name, s := range p.servers {
		for _, t := range s.Tools {
			refs = append(refs, ToolRef{Server: name, Tool: t})
		}
	}
	return refs
}

func (p *Pool) CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (*CallToolResult, error) {
	p.mu.RLock()
	s, ok := p.servers[serverName]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mcp server %s not connected", serverName)
	}
	return s.Client.CallTool(ctx, toolName, args)
}

func (p *Pool) AllResources() []ResourceRef {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var refs []ResourceRef
	for name, s := range p.servers {
		for _, r := range s.Resources {
			refs = append(refs, ResourceRef{Server: name, Resource: r})
		}
	}
	return refs
}

func (p *Pool) ReadResource(ctx context.Context, serverName, uri string) (*ReadResourceResult, error) {
	p.mu.RLock()
	s, ok := p.servers[serverName]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mcp server %s not connected", serverName)
	}
	return s.Client.ReadResource(ctx, uri)
}

type ToolRef struct {
	Server string
	Tool   Tool
}

type ResourceRef struct {
	Server   string
	Resource Resource
}

func (ms *ManagedServer) Close() {
	ms.Connected = false
	if ms.Client != nil {
		_ = ms.Client.Close()
	}
	// Note: transport is already closed by Client.Close(), no double-close
}

// LoadFromConfig connects to all servers defined in the config map.
func (p *Pool) LoadFromConfig(ctx context.Context, servers map[string]ServerConfig) {
	for name, cfg := range servers {
		if err := p.Connect(ctx, name, cfg); err != nil {
			logF("MCP: %s: %v", name, err)
		}
	}
}

// logF emits diagnostic messages only when the global log level is Debug,
// so MCP connection problems stay hidden in normal (release) runs.
func logF(format string, args ...any) {
	log.Debugf(format, args...)
}
