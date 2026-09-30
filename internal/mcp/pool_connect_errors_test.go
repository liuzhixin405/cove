package mcp

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A tools/list that fails for a reason other than "not supported" used to be
// swallowed: the server was published connected with zero tools and no error,
// and /mcp list gave no hint why the model saw none of its tools. The entry
// stays connected (the handshake worked) but carries the error.
func TestConnectKeepsToolsListErrorOnServer(t *testing.T) {
	srv := newAutoServer("t")
	srv.failToolsList("-32603", "database unavailable")
	p := NewPool()
	p.dial = func(string, ServerConfig) (Transport, error) { return srv, nil }
	if err := p.Connect(context.Background(), "srv", ServerConfig{}); err != nil {
		t.Fatalf("Connect: %v (a failed tools/list is recorded, not returned)", err)
	}
	defer p.DisconnectAll()

	servers := p.AllServers()
	if len(servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(servers))
	}
	ms := servers[0]
	if !ms.Connected {
		t.Fatal("server should still be connected: initialize succeeded")
	}
	if !strings.Contains(ms.Err, "tools/list") || !strings.Contains(ms.Err, "database unavailable") {
		t.Fatalf("Err = %q, want it to name tools/list and the server's message", ms.Err)
	}
	if len(ms.Tools) != 0 {
		t.Fatalf("tools = %+v, want none", ms.Tools)
	}
}

// A capability the server simply did not declare is not an error.
func TestConnectIgnoresUnsupportedToolsList(t *testing.T) {
	srv := newAutoServer("t")
	srv.failToolsList("-32601", "server does not support tools")
	p := NewPool()
	p.dial = func(string, ServerConfig) (Transport, error) { return srv, nil }
	if err := p.Connect(context.Background(), "srv", ServerConfig{}); err != nil {
		t.Fatal(err)
	}
	defer p.DisconnectAll()
	if ms := p.AllServers()[0]; ms.Err != "" {
		t.Fatalf("Err = %q, want none for an undeclared capability", ms.Err)
	}
}

// A stdio server that dies twice: the first death is followed by the one
// automatic reconnect, the second is final. After it the entry must be closed
// (Connected=false), say why, and both child processes must be reaped. The
// second dead client used to be left as it was: never Wait()ed, so its
// process entry stayed a zombie and its pipes stayed open until exit.
func TestWatchReapsServerAfterSecondDeath(t *testing.T) {
	old := reconnectDelay
	reconnectDelay = 10 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = old })

	var mu sync.Mutex
	var transports []*stdioTransport
	p := NewPool()
	p.dial = func(string, ServerConfig) (Transport, error) {
		tr, err := NewStdioTransport(os.Args[0], helperArgs(), helperEnv("rpc"))
		if err != nil {
			return nil, err
		}
		mu.Lock()
		transports = append(transports, tr)
		mu.Unlock()
		return tr, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.Connect(ctx, "srv", ServerConfig{}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer p.DisconnectAll()

	nth := func(i int) *stdioTransport {
		mu.Lock()
		defer mu.Unlock()
		if len(transports) <= i {
			return nil
		}
		return transports[i]
	}

	// First death: killed from outside, reconnected once.
	if err := nth(0).cmd.Process.Kill(); err != nil {
		t.Fatalf("kill first child: %v", err)
	}
	waitFor(t, "reconnect after the first death", func() bool {
		s := p.AllServers()
		return nth(1) != nil && len(s) == 1 && s[0].Connected && s[0].Err == ""
	})
	// Connect evicts the dead predecessor through Close, which Wait()s it.
	if nth(0).cmd.ProcessState == nil {
		t.Fatal("first child was not reaped when it was replaced")
	}

	// Second death: no more reconnects; the entry is closed and reaped.
	if err := nth(1).cmd.Process.Kill(); err != nil {
		t.Fatalf("kill second child: %v", err)
	}
	waitFor(t, "the entry to be marked dead after the second death", func() bool {
		s := p.AllServers()
		return len(s) == 1 && s[0].Err != ""
	})
	ms := p.AllServers()[0]
	if ms.Connected {
		t.Fatal("server still reported connected after its second death")
	}
	if !strings.Contains(ms.Err, "connection lost") {
		t.Fatalf("Err = %q, want it to say the connection was lost", ms.Err)
	}
	if nth(1).cmd.ProcessState == nil {
		t.Fatal("second child was not Wait()ed: it is left as a zombie")
	}
	if nth(2) != nil {
		t.Fatal("a third dial happened; only one automatic reconnect is allowed")
	}
}
