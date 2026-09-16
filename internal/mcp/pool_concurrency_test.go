package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingCloseTransport parks inside Close until the test releases it, which
// makes "is the pool lock held across a close?" observable without sleeps.
type blockingCloseTransport struct {
	entered chan struct{}
	release chan struct{}
	closes  int32
}

func newBlockingCloseTransport(capacity int) *blockingCloseTransport {
	return &blockingCloseTransport{
		entered: make(chan struct{}, capacity),
		release: make(chan struct{}),
	}
}

func (b *blockingCloseTransport) Send(context.Context, any) error { return nil }

func (b *blockingCloseTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingCloseTransport) Close() error {
	atomic.AddInt32(&b.closes, 1)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return nil
}

func managedWithTransport(name string, tr Transport, tools []Tool) *ManagedServer {
	return &ManagedServer{
		Name:      name,
		Client:    NewClient(tr),
		Transport: tr,
		Tools:     tools,
		Connected: true,
	}
}

// TestPool_DisconnectAllClosesServersConcurrently: N servers must be closed in
// parallel outside the lock. Serialized, N unresponsive servers would each burn
// the full close grace period in turn.
func TestPool_DisconnectAllClosesServersConcurrently(t *testing.T) {
	const n = 5
	p := NewPool()
	tr := newBlockingCloseTransport(n)
	for i := 0; i < n; i++ {
		name := string(rune('a' + i))
		p.servers[name] = managedWithTransport(name, tr, nil)
	}

	done := make(chan struct{})
	go func() {
		p.DisconnectAll()
		close(done)
	}()

	// All N closes must be in flight at the same time.
	deadline := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-tr.entered:
		case <-deadline:
			t.Fatalf("only %d of %d server closes started; DisconnectAll is serializing them", i, n)
		}
	}
	close(tr.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DisconnectAll did not return")
	}

	if got := len(p.AllServers()); got != 0 {
		t.Fatalf("pool still holds %d servers after DisconnectAll", got)
	}
	if got := atomic.LoadInt32(&tr.closes); got != n {
		t.Fatalf("transport Close called %d times; want %d", got, n)
	}
}

// TestPool_DisconnectDoesNotHoldLockDuringClose: Disconnect unregisters under
// the lock but closes afterwards, so a slow child reap cannot freeze the pool.
func TestPool_DisconnectDoesNotHoldLockDuringClose(t *testing.T) {
	p := NewPool()
	tr := newBlockingCloseTransport(1)
	p.servers["slow"] = managedWithTransport("slow", tr, nil)
	p.servers["fast"] = managedWithTransport("fast", newScriptedTransport(), []Tool{{Name: "t"}})

	done := make(chan struct{})
	go func() {
		p.Disconnect("slow")
		close(done)
	}()

	select {
	case <-tr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect never reached the transport close")
	}

	read := make(chan int, 1)
	go func() { read <- len(p.AllServers()) }()
	select {
	case got := <-read:
		if got != 1 {
			t.Fatalf("AllServers returned %d servers mid-Disconnect; want only the remaining one", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AllServers blocked while Disconnect was closing another server: the pool lock is held across Close")
	}

	close(tr.release)
	<-done
}

// TestPool_ConnectDoesNotHoldLockWhileClosingStaleServer covers Connect's
// replacement path: evicting a dead server must not pin the pool lock for the
// duration of that server's shutdown.
func TestPool_ConnectDoesNotHoldLockWhileClosingStaleServer(t *testing.T) {
	p := NewPool()
	stale := newBlockingCloseTransport(1)
	ms := managedWithTransport("A", stale, nil)
	ms.Connected = false // dead, so Connect replaces it
	p.servers["A"] = ms
	p.servers["B"] = managedWithTransport("B", newScriptedTransport(), []Tool{{Name: "b-tool"}})

	connectDone := make(chan error, 1)
	go func() {
		connectDone <- p.Connect(context.Background(), "A",
			ServerConfig{Command: "cove-definitely-not-a-real-binary-xyz"})
	}()

	select {
	case <-stale.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Connect never closed the stale server")
	}

	// Unrelated pool operations must proceed while A is shutting down.
	type reads struct {
		servers int
		tools   int
	}
	readCh := make(chan reads, 1)
	go func() {
		r := reads{servers: len(p.AllServers()), tools: len(p.AllTools())}
		p.Disconnect("B")
		readCh <- r
	}()

	select {
	case r := <-readCh:
		if r.tools != 1 {
			t.Fatalf("AllTools returned %d tools mid-Connect; want 1", r.tools)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AllServers/AllTools/Disconnect blocked while Connect shut a stale server down: the pool lock is held across Close")
	}

	close(stale.release)
	if err := <-connectDone; err == nil {
		t.Fatal("Connect with a nonexistent command returned nil error")
	}
	if _, ok := p.servers["A"]; ok {
		t.Fatal("failed Connect left the stale server registered")
	}
}

// TestPool_AllServersReturnsSnapshots: callers read the returned metadata after
// the lock is released, so mutating what they got must never reach the pool.
func TestPool_AllServersReturnsSnapshots(t *testing.T) {
	p := NewPool()
	tr := newScriptedTransport()
	p.servers["srv"] = managedWithTransport("srv", tr, []Tool{{Name: "original"}})
	p.servers["srv"].Resources = []Resource{{URI: "mem://a", Name: "a"}}

	snap := p.AllServers()
	if len(snap) != 1 {
		t.Fatalf("AllServers returned %d entries; want 1", len(snap))
	}
	if snap[0] == p.servers["srv"] {
		t.Fatal("AllServers handed out the pool's live *ManagedServer instead of a copy")
	}

	snap[0].Connected = false
	snap[0].Name = "mutated"
	snap[0].Tools[0].Name = "hijacked"
	snap[0].Tools = append(snap[0].Tools, Tool{Name: "extra"})
	snap[0].Resources[0].URI = "mem://hijacked"

	live := p.servers["srv"]
	if !live.Connected {
		t.Fatal("mutating the snapshot flipped the pool's Connected flag")
	}
	if live.Name != "srv" {
		t.Fatalf("pool server name = %q; the snapshot's Name write leaked", live.Name)
	}
	if len(live.Tools) != 1 || live.Tools[0].Name != "original" {
		t.Fatalf("pool tools = %+v; the snapshot shares the Tools backing array", live.Tools)
	}
	if live.Resources[0].URI != "mem://a" {
		t.Fatalf("pool resources = %+v; the snapshot shares the Resources backing array", live.Resources)
	}

	refs := p.AllTools()
	if len(refs) != 1 || refs[0].Tool.Name != "original" || refs[0].Server != "srv" {
		t.Fatalf("AllTools = %+v; want the untouched original tool", refs)
	}
	rrefs := p.AllResources()
	if len(rrefs) != 1 || rrefs[0].Resource.URI != "mem://a" {
		t.Fatalf("AllResources = %+v", rrefs)
	}
}

// TestPool_ConcurrentReadsAndDisconnects must be race-free under -race, and
// every snapshot handed out must be internally consistent (never a
// half-populated record).
func TestPool_ConcurrentReadsAndDisconnects(t *testing.T) {
	const servers = 8
	p := NewPool()
	names := make([]string, 0, servers)
	for i := 0; i < servers; i++ {
		name := "srv" + string(rune('0'+i))
		names = append(names, name)
		p.servers[name] = managedWithTransport(name, newScriptedTransport(), []Tool{{Name: name + "-tool"}})
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad atomic.Int64

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, s := range p.AllServers() {
					if s.Name == "" || len(s.Tools) != 1 || s.Tools[0].Name != s.Name+"-tool" {
						bad.Add(1)
					}
				}
				for _, ref := range p.AllTools() {
					if ref.Tool.Name != ref.Server+"-tool" {
						bad.Add(1)
					}
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, name := range names {
			p.Disconnect(name)
		}
		close(stop)
	}()

	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d inconsistent snapshots observed during concurrent Disconnect", n)
	}
	if got := len(p.AllServers()); got != 0 {
		t.Fatalf("%d servers left after disconnecting all of them", got)
	}
}

func TestPool_ConnectRejectsBadTransportConfig(t *testing.T) {
	p := NewPool()
	ctx := context.Background()

	cases := []struct {
		name string
		cfg  ServerConfig
		want string
	}{
		{"sse-no-url", ServerConfig{Type: "sse"}, "url"},
		{"http-no-url", ServerConfig{Type: "http"}, "url"},
		{"streamable-no-url", ServerConfig{Type: "streamablehttp"}, "url"},
		{"stdio-no-command", ServerConfig{}, "command is required"},
		{"stdio-shell", ServerConfig{Command: "bash"}, "shell wrappers"},
		{"stdio-injection", ServerConfig{Command: "node; rm -rf /"}, "shell control characters"},
	}
	for _, tc := range cases {
		err := p.Connect(ctx, tc.name, tc.cfg)
		if err == nil {
			t.Fatalf("%s: Connect returned nil error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %q; want it to mention %q", tc.name, err, tc.want)
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Fatalf("%s: error = %q; want it to name the server", tc.name, err)
		}
		if _, ok := p.servers[tc.name]; ok {
			t.Fatalf("%s: a failed Connect registered the server anyway", tc.name)
		}
	}
}

func TestPool_ConnectIsNoOpForAlreadyConnectedServer(t *testing.T) {
	p := NewPool()
	tr := newScriptedTransport()
	existing := managedWithTransport("A", tr, []Tool{{Name: "keep"}})
	p.servers["A"] = existing

	// An invalid config would normally fail; a live server must short-circuit
	// before the transport is ever built.
	if err := p.Connect(context.Background(), "A", ServerConfig{Command: "bash"}); err != nil {
		t.Fatalf("Connect for a live server = %v; want nil", err)
	}
	if p.servers["A"] != existing {
		t.Fatal("Connect replaced a live server")
	}
	if tr.closeCount() != 0 {
		t.Fatalf("Connect closed the live server's transport %d times", tr.closeCount())
	}
}

func TestPool_CallToolAndReadResourceRejectUnknownServer(t *testing.T) {
	p := NewPool()
	ctx := context.Background()

	if _, err := p.CallTool(ctx, "ghost", "tool", nil); err == nil {
		t.Fatal("CallTool on an unknown server returned nil error")
	} else if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("error = %q", err)
	}

	if _, err := p.ReadResource(ctx, "ghost", "mem://x"); err == nil {
		t.Fatal("ReadResource on an unknown server returned nil error")
	} else if !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("error = %q", err)
	}
}

// TestPool_LoadFromConfigOverStdio is the end-to-end path: a real child
// process, the handshake, tool/resource discovery and dispatch through the
// pool. The rejected shell entry must not stop the good server from loading.
func TestPool_LoadFromConfigOverStdio(t *testing.T) {
	p := NewPool()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p.LoadFromConfig(ctx, map[string]ServerConfig{
		"helper": {Command: os.Args[0], Args: helperArgs(), Env: helperEnv("rpc")},
		"evil":   {Command: "sh"},
	})
	defer p.DisconnectAll()

	all := p.AllServers()
	if len(all) != 1 || all[0].Name != "helper" || !all[0].Connected {
		t.Fatalf("AllServers = %+v; want only a connected helper", all)
	}

	tools := p.AllTools()
	if len(tools) != 1 || tools[0].Server != "helper" || tools[0].Tool.Name != "echo" {
		t.Fatalf("AllTools = %+v", tools)
	}
	if got := ToolName(tools[0].Server, tools[0].Tool.Name); got != "mcp__helper__echo" {
		t.Fatalf("ToolName = %q", got)
	}

	res, err := p.CallTool(ctx, "helper", "echo", map[string]any{"text": "pool"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "echo:pool" {
		t.Fatalf("CallTool content = %+v", res.Content)
	}

	rr, err := p.ReadResource(ctx, "helper", "mem://note")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(rr.Contents) != 1 || rr.Contents[0].Text != "body of mem://note" {
		t.Fatalf("ReadResource = %+v", rr.Contents)
	}

	// DisconnectAll must reap the child.
	live := p.servers["helper"]
	stdio, ok := live.Transport.(*stdioTransport)
	if !ok {
		t.Fatalf("transport type = %T; want *stdioTransport", live.Transport)
	}
	p.DisconnectAll()
	if stdio.cmd.ProcessState == nil {
		t.Fatal("DisconnectAll left the child process unreaped")
	}
	if len(p.AllServers()) != 0 {
		t.Fatal("DisconnectAll left servers registered")
	}
}

func TestValidateSTDIOCommandRejectsShellWrappersInPathsAndCases(t *testing.T) {
	rejected := []string{
		"SH", "Bash", "  bash  ", "PowerShell.EXE",
		"/bin/sh", "/usr/bin/bash", "/usr/local/bin/zsh", "/bin/fish", "/bin/ksh",
		"C:/Windows/System32/cmd.exe", "C:/Program Files/PowerShell/7/pwsh.exe",
	}
	for _, command := range rejected {
		if err := validateSTDIOCommand(command); err == nil {
			t.Fatalf("validateSTDIOCommand(%q) = nil; want a rejection", command)
		}
	}

	for _, command := range []string{"", "   ", "\t"} {
		if err := validateSTDIOCommand(command); err == nil {
			t.Fatalf("validateSTDIOCommand(%q) = nil; want 'command is required'", command)
		}
	}

	for _, command := range []string{
		"node", "npx", "uvx", "docker", "python3",
		"/usr/bin/node", "C:/Program Files/nodejs/node.exe",
		"shell-like-name", "bashful",
	} {
		if err := validateSTDIOCommand(command); err != nil {
			t.Fatalf("validateSTDIOCommand(%q) = %v; want nil", command, err)
		}
	}
}

func TestNewSTDIOTransportNamesTheServerOnValidationFailure(t *testing.T) {
	tr, err := NewSTDIOTransport("my-server", ServerConfig{Command: "pwsh.exe"})
	if err == nil {
		tr.Close()
		t.Fatal("NewSTDIOTransport accepted pwsh.exe")
	}
	if !strings.Contains(err.Error(), "my-server") {
		t.Fatalf("error = %q; want it to name the server", err)
	}
}
