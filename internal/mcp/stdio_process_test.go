package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	helperEnvFlag = "COVE_MCP_HELPER_PROCESS"
	helperEnvMode = "COVE_MCP_HELPER_MODE"
)

// TestHelperProcess is the child-process entry point used by the stdio
// transport tests (standard os.Args[0] + -test.run re-exec pattern). It is a
// no-op unless the parent set COVE_MCP_HELPER_PROCESS=1, so no real MCP server
// binary is needed anywhere in this package's tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvFlag) != "1" {
		return
	}
	switch os.Getenv(helperEnvMode) {
	case "drain":
		// Well-behaved server: exits as soon as its stdin is closed.
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "ignore-stdin":
		// Misbehaving server: never notices the stdin close, so it has to be
		// killed after the close grace period.
		time.Sleep(60 * time.Second)
	case "rpc":
		helperServeRPC()
	case "tree":
		helperSpawnTree()
	case "stderr-flood":
		helperStderrFlood()
	}
	os.Exit(0)
}

// helperServeRPC speaks just enough JSON-RPC over stdio to complete a
// handshake and answer tools/resources requests.
func helperServeRPC() {
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	out := bufio.NewWriter(os.Stdout)

	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue // notification
		}

		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2024-11-05","capabilities":{"tools":{},"resources":{}},"serverInfo":{"name":"helper","version":"9.9.9"}}`
		case "tools/list":
			result = `{"tools":[{"name":"echo","description":"echoes text","inputSchema":{"type":"object"}}]}`
		case "resources/list":
			result = `{"resources":[{"uri":"mem://note","name":"note"}]}`
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text, _ := json.Marshal(fmt.Sprintf("%s:%v", p.Name, p.Arguments["text"]))
			result = fmt.Sprintf(`{"content":[{"type":"text","text":%s}]}`, text)
		case "resources/read":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text, _ := json.Marshal("body of " + p.URI)
			result = fmt.Sprintf(`{"contents":[{"type":"text","text":%s}]}`, text)
		default:
			fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"unknown method %s"}}`+"\n", *req.ID, req.Method)
			out.Flush()
			continue
		}
		fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", *req.ID, result)
		out.Flush()
	}
}

func helperArgs() []string {
	return []string{"-test.run=^TestHelperProcess$"}
}

func helperEnv(mode string) map[string]string {
	return map[string]string{helperEnvFlag: "1", helperEnvMode: mode}
}

func newHelperTransport(t *testing.T, mode string) *stdioTransport {
	t.Helper()
	tr, err := NewStdioTransport(os.Args[0], helperArgs(), helperEnv(mode))
	if err != nil {
		t.Fatalf("NewStdioTransport(%s): %v", mode, err)
	}
	return tr
}

// TestStdioTransport_CloseReapsChildAndIsIdempotent: Close is reachable from
// both Pool.Connect's error path and Client.Close, so it must be safe to call
// twice, and it must Wait() the child so no zombie is left behind.
func TestStdioTransport_CloseReapsChildAndIsIdempotent(t *testing.T) {
	tr := newHelperTransport(t, "drain")

	if tr.cmd.ProcessState != nil {
		t.Fatal("child already reaped before Close")
	}

	start := time.Now()
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)

	if tr.cmd.ProcessState == nil {
		t.Fatal("Close did not Wait() the child: its process entry leaks as a zombie")
	}
	if !tr.cmd.ProcessState.Exited() {
		t.Fatal("child did not exit")
	}
	if code := tr.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("child exit code = %d; want 0 (a clean exit after stdin closed)", code)
	}
	if elapsed >= stdioCloseGrace {
		t.Fatalf("Close took %v; a server that exits on stdin close must not wait out the %v grace period", elapsed, stdioCloseGrace)
	}

	// Second Close must not panic and must not Wait() twice (a second Wait
	// returns "Wait was already called" and would replace waitErr).
	state := tr.cmd.ProcessState
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close returned %v; want the first result (nil)", err)
	}
	if tr.cmd.ProcessState != state {
		t.Fatal("second Close re-Waited the child")
	}
}

// TestStdioTransport_CloseKillsUnresponsiveChild: a server that ignores the
// stdin close must be killed and reaped rather than hanging the shutdown.
func TestStdioTransport_CloseKillsUnresponsiveChild(t *testing.T) {
	tr := newHelperTransport(t, "ignore-stdin")

	start := time.Now()
	_ = tr.Close() // Wait reports a kill signal here; the error is expected
	elapsed := time.Since(start)

	if tr.cmd.ProcessState == nil {
		t.Fatal("Close did not reap the killed child")
	}
	if elapsed > stdioCloseGrace+5*time.Second {
		t.Fatalf("Close took %v; want roughly the %v grace period", elapsed, stdioCloseGrace)
	}
	if tr.cmd.ProcessState.Success() {
		t.Fatal("child reported a successful exit; it was supposed to be killed")
	}

	// Idempotent even on the kill path.
	if err := tr.Close(); err != nil && !strings.Contains(err.Error(), "signal") && !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("second Close returned an unexpected error: %v", err)
	}
}

func TestNewStdioTransport_MissingBinaryFails(t *testing.T) {
	tr, err := NewStdioTransport("cove-definitely-not-a-real-binary-xyz", nil, nil)
	if err == nil {
		tr.Close()
		t.Fatal("NewStdioTransport succeeded for a nonexistent command")
	}
	if !strings.Contains(err.Error(), "cove-definitely-not-a-real-binary-xyz") {
		t.Fatalf("error = %q; want it to name the command", err)
	}
}

// TestClientOverStdio_FullHandshake exercises Client against a real child
// process over real pipes: handshake, capability discovery, a tool call and a
// resource read, then a clean shutdown that reaps the child.
func TestClientOverStdio_FullHandshake(t *testing.T) {
	tr := newHelperTransport(t, "rpc")
	c := NewClient(tr)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		tr.Close()
		t.Fatalf("Connect: %v", err)
	}
	if info := c.ServerInfo(); info.Name != "helper" || info.Version != "9.9.9" {
		t.Fatalf("ServerInfo = %+v; want {helper 9.9.9}", info)
	}

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v; want one tool named echo", tools)
	}

	resources, err := c.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 1 || resources[0].URI != "mem://note" {
		t.Fatalf("ListResources = %+v", resources)
	}

	res, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "echo:hi" {
		t.Fatalf("CallTool content = %+v; want echo:hi", res.Content)
	}

	rr, err := c.ReadResource(ctx, "mem://note")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(rr.Contents) != 1 || rr.Contents[0].Text != "body of mem://note" {
		t.Fatalf("ReadResource contents = %+v", rr.Contents)
	}

	// An unknown method must surface the server's JSON-RPC error.
	if err := c.Call(ctx, "does/not/exist", nil, nil); err == nil {
		t.Fatal("Call to an unknown method returned nil error")
	} else if !strings.Contains(err.Error(), "-32601") {
		t.Fatalf("error = %q; want the server's -32601", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if tr.cmd.ProcessState == nil {
		t.Fatal("Client.Close left the child process unreaped")
	}
}
