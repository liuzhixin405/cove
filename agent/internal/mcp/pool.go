package mcp

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
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

func (p *Pool) Connect(ctx context.Context, name string, cfg ServerConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.servers[name]; ok {
		if existing.Connected {
			return nil
		}
		existing.Close()
	}

	var transport Transport
	var err error

	switch cfg.Type {
	case "sse":
		if cfg.URL != "" {
			transport, err = NewSSETransport(cfg.URL)
		} else {
			err = fmt.Errorf("sse type requires 'url' in config")
		}
	case "http":
		if cfg.URL != "" {
			transport, err = NewSSETransport(cfg.URL)
		} else {
			err = fmt.Errorf("http type requires 'url' in config")
		}
	default:
		transport, err = NewSTDIOTransport(name, cfg)
	}

	if err != nil {
		return fmt.Errorf("transport for %s: %w", name, err)
	}

	client := NewClient(transport)
	if err := client.Connect(ctx); err != nil {
		transport.Close()
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

	p.servers[name] = ms
	return nil
}

func NewSTDIOTransport(name string, cfg ServerConfig) (Transport, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("server %s: command is required", name)
	}
	args := cfg.Args
	env := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		env[k] = v
	}
	return NewStdioTransport(cfg.Command, args, env)
}

func (p *Pool) Disconnect(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.servers[name]; ok {
		s.Close()
		delete(p.servers, name)
	}
}

func (p *Pool) DisconnectAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, s := range p.servers {
		s.Close()
		delete(p.servers, name)
	}
}

func (p *Pool) Server(name string) *ManagedServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.servers[name]
}

func (p *Pool) AllServers() []*ManagedServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var result []*ManagedServer
	for _, s := range p.servers {
		result = append(result, s)
	}
	return result
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
		ms.Client.Close()
	}
	// Note: transport is already closed by Client.Close(), no double-close
}

func ConnectFromConfig(ctx context.Context, servers map[string]ServerConfig) *Pool {
	pool := NewPool()
	for name, cfg := range servers {
		if err := pool.Connect(ctx, name, cfg); err != nil {
			logF("MCP: %s: %v", name, err)
		}
	}
	return pool
}

var logger = log.New(os.Stderr, "", 0)

func logF(format string, args ...any) {
	logger.Printf(format, args...)
}
