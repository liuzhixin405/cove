package mcp

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// requireFailedEntry asserts that a failed Connect left, at most, a not-live
// entry carrying its error. (Failed servers used to be dropped entirely; they
// are now listed so /mcp can show why they failed.)
func requireFailedEntry(t *testing.T, p *Pool, name string) {
	t.Helper()
	p.mu.RLock()
	s, ok := p.servers[name]
	p.mu.RUnlock()
	if !ok {
		return
	}
	if s.live() || s.Client != nil || s.Err == "" {
		t.Fatalf("%s: failed Connect left entry {live:%v client:%v err:%q}; want a dead entry with the error", name, s.live(), s.Client != nil, s.Err)
	}
}

// TestPool_TransportTypeSpellings: the docs spell the HTTP transport
// "streamable_http", other MCP clients' configs use "http", and the SDKs say
// "streamable-http" / "streamableHttp". Every spelling must reach the HTTP
// dialer; before, anything but "http"/"streamablehttp" silently fell through
// to stdio and failed with the baffling "command is required".
func TestPool_TransportTypeSpellings(t *testing.T) {
	for _, typ := range []string{"http", "streamablehttp", "streamable_http", "streamable-http", "streamableHttp", "Streamable-HTTP"} {
		err := NewPool().Connect(context.Background(), "s", ServerConfig{Type: typ})
		if err == nil || !strings.Contains(err.Error(), "requires 'url'") {
			t.Errorf("type %q: error = %v; want the HTTP transport's missing-url error", typ, err)
		}
	}
}

// TestPool_UnknownTransportTypeIsReported: a typo in "type" must say so
// instead of being treated as a stdio server with no command.
func TestPool_UnknownTransportTypeIsReported(t *testing.T) {
	err := NewPool().Connect(context.Background(), "s", ServerConfig{Type: "websocket", URL: "ws://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "unknown transport type") || !strings.Contains(err.Error(), "websocket") {
		t.Fatalf("error = %v; want it to name the unknown transport type", err)
	}
	for _, typ := range []string{"", "stdio", "STDIO"} {
		err := NewPool().Connect(context.Background(), "s", ServerConfig{Type: typ})
		if err == nil || !strings.Contains(err.Error(), "command is required") {
			t.Errorf("type %q: error = %v; want the stdio dialer's error", typ, err)
		}
	}
}

// TestPool_ListingsAreSorted: the mcp tool's description is rebuilt from
// AllTools on every request. Map iteration order made it differ from turn to
// turn, which changes the tool definitions in the prompt prefix and defeats
// provider prompt caching (DeepSeek and Anthropic both cache by prefix).
func TestPool_ListingsAreSorted(t *testing.T) {
	p := NewPool()
	for _, name := range []string{"zeta", "alpha", "mid", "beta", "omega", "gamma"} {
		ms := managedWithTransport(name, newScriptedTransport(), []Tool{{Name: name + "-2"}, {Name: name + "-1"}})
		ms.Resources = []Resource{{URI: name + "://r"}}
		p.servers[name] = ms
	}

	for i := 0; i < 20; i++ {
		var servers []string
		for _, s := range p.AllServers() {
			servers = append(servers, s.Name)
		}
		if !sort.StringsAreSorted(servers) {
			t.Fatalf("AllServers order = %v; want sorted by name", servers)
		}

		var toolServers []string
		for _, r := range p.AllTools() {
			toolServers = append(toolServers, r.Server)
		}
		if !sort.StringsAreSorted(toolServers) {
			t.Fatalf("AllTools server order = %v; want sorted by server", toolServers)
		}
		// Within a server, the server's own order is kept.
		if refs := p.AllTools(); refs[0].Tool.Name != "alpha-2" || refs[1].Tool.Name != "alpha-1" {
			t.Fatalf("AllTools = %+v; want alpha's tools first, in the server's order", refs[:2])
		}

		var resServers []string
		for _, r := range p.AllResources() {
			resServers = append(resServers, r.Server)
		}
		if !sort.StringsAreSorted(resServers) {
			t.Fatalf("AllResources order = %v; want sorted by server", resServers)
		}
	}
}

// TestPool_LoadFromConfigConnectsServersConcurrently: startup gives MCP one
// shared deadline. Connecting serially, one server that never answers
// initialize burned that whole budget and every server after it failed with
// an expired context - one broken entry took all the others down with it.
//
// The dialer here answers no initialize until it has seen EVERY server's
// initialize, so a serial loop deadlocks until the context expires.
func TestPool_LoadFromConfigConnectsServersConcurrently(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	transports := map[string]*scriptedTransport{}
	for _, n := range names {
		transports[n] = newScriptedTransport()
	}
	p := NewPool()
	p.dial = func(name string, cfg ServerConfig) (Transport, error) {
		return transports[name], nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loaded := make(chan struct{})
	go func() {
		p.LoadFromConfig(ctx, map[string]ServerConfig{"a": {}, "b": {}, "c": {}, "d": {}})
		close(loaded)
	}()

	ids := map[string]int{}
	for _, n := range names {
		select {
		case m := <-transports[n].sent:
			if m.Method != "initialize" {
				t.Fatalf("%s: first message %q; want initialize", n, m.Method)
			}
			ids[n] = *m.ID
		case <-time.After(3 * time.Second):
			t.Fatalf("server %s was not dialled while the others were still handshaking: LoadFromConfig connects serially", n)
		}
	}
	for _, n := range names {
		transports[n].incoming <- respJSON(ids[n], `{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"`+n+`","version":"1"}}`)
	}
	select {
	case <-loaded:
	case <-time.After(5 * time.Second):
		t.Fatal("LoadFromConfig did not return")
	}
	if got := len(p.AllServers()); got != len(names) {
		t.Fatalf("%d servers connected; want %d", got, len(names))
	}
	p.DisconnectAll()
}

// TestPool_FailedServerIsListedWithItsError: a server that fails to start used
// to vanish - the error went to the debug log only, so "/mcp list" just said
// there were no servers and the user had no way to learn why.
func TestPool_FailedServerIsListedWithItsError(t *testing.T) {
	p := NewPool()
	p.LoadFromConfig(context.Background(), map[string]ServerConfig{"broken": {Type: "stdio"}})

	servers := p.AllServers()
	if len(servers) != 1 {
		t.Fatalf("AllServers = %d entries; want the failed server listed", len(servers))
	}
	s := servers[0]
	if s.Name != "broken" || s.Connected || !strings.Contains(s.Err, "command is required") {
		t.Fatalf("listed server = {Name:%q Connected:%v Err:%q}; want broken, not connected, with the dial error", s.Name, s.Connected, s.Err)
	}
	if len(p.AllTools()) != 0 {
		t.Fatal("a failed server contributed tools")
	}

	// Calls against it must report the failure, not panic on the nil client.
	if _, err := p.CallTool(context.Background(), "broken", "x", nil); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("CallTool error = %v; want the recorded connect error", err)
	}
	if _, err := p.ReadResource(context.Background(), "broken", "x://y"); err == nil {
		t.Fatal("ReadResource on a failed server returned nil error")
	}

	// A later successful connect replaces the failed entry.
	p.dial = func(string, ServerConfig) (Transport, error) {
		tr := newScriptedTransport()
		go func() {
			m := <-tr.sent
			tr.incoming <- respJSON(*m.ID, `{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"ok","version":"1"}}`)
		}()
		return tr, nil
	}
	if err := p.Connect(context.Background(), "broken", ServerConfig{}); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if s := p.AllServers()[0]; !s.Connected || s.Err != "" {
		t.Fatalf("after reconnect: Connected=%v Err=%q; want a clean live entry", s.Connected, s.Err)
	}
	p.DisconnectAll()
}
