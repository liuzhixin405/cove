package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// startLoop runs the client's receive loop and returns a channel closed when
// the loop has exited.
func startLoop(c *Client) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		c.receiveLoop()
		close(done)
	}()
	return done
}

// TestClient_CallFailsFastAfterServerDies: when the server goes away (stdio
// child crashed, SSE stream dropped) the receive loop exits and nothing can
// answer a request any more. A Call made after that must fail at once instead
// of sending into the void and hanging until its context expires - for the
// engine's tool context, that is until the user interrupts.
func TestClient_CallFailsFastAfterServerDies(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	loopDone := startLoop(c)

	_ = tr.Close() // the server died: Receive now returns EOF
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("receiveLoop did not exit after the transport died")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := c.Call(ctx, "tools/call", nil, nil)
	if err == nil {
		t.Fatal("Call on a dead connection returned nil error")
	}
	if errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("Call on a dead connection hung until its deadline (%v) instead of failing fast", err)
	}
	if c.Alive() {
		t.Fatal("Alive() = true for a client whose receive loop has exited")
	}
}

// TestPool_ConnectReplacesDeadServer: "/mcp connect X" after X crashed used to
// answer "connected" and do nothing, because the pool still had X marked
// Connected. It must instead evict the dead server and dial again.
func TestPool_ConnectReplacesDeadServer(t *testing.T) {
	tr := newScriptedTransport()
	p := NewPool()
	ms := managedWithTransport("X", tr, []Tool{{Name: "t"}})
	p.servers["X"] = ms
	loopDone := startLoop(ms.Client)
	_ = tr.Close()
	<-loopDone

	for _, s := range p.AllServers() {
		if s.Name == "X" && s.Connected {
			t.Fatal("AllServers reports a crashed server as connected")
		}
	}

	// An empty stdio command fails fast in the dialer, which proves Connect
	// really tried to dial instead of returning early.
	err := p.Connect(context.Background(), "X", ServerConfig{})
	if err == nil {
		t.Fatal("Connect returned nil for a dead server without reconnecting")
	}
	if !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("Connect error = %v; want the dial error of the new attempt", err)
	}
}

// TestClient_AnswersServerPing: the spec requires the receiver of a ping to
// answer promptly, and servers use it as a keepalive - some drop a client that
// never answers. Ids may be strings, and the reply must echo the id verbatim.
func TestClient_AnswersServerPing(t *testing.T) {
	for _, id := range []string{`5`, `"srv-ping-1"`} {
		tr := newScriptedTransport()
		c := NewClient(tr)
		startLoop(c)

		tr.incoming <- json.RawMessage(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
		reply := tr.nextSent(t)

		var got struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if err := json.Unmarshal(reply.Raw, &got); err != nil {
			t.Fatalf("reply %s: %v", reply.Raw, err)
		}
		if string(got.ID) != id {
			t.Fatalf("ping reply id = %s; want %s echoed (%s)", got.ID, id, reply.Raw)
		}
		if got.Error != nil || string(got.Result) != "{}" {
			t.Fatalf("ping reply = %s; want an empty result", reply.Raw)
		}
		c.Close()
	}
}

// TestClient_RejectsUnsupportedServerRequest: a server request the client does
// not implement (sampling, roots, elicitation) must get "method not found"
// rather than silence, or the server waits on it forever.
func TestClient_RejectsUnsupportedServerRequest(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	startLoop(c)
	defer c.Close()

	tr.incoming <- json.RawMessage(`{"jsonrpc":"2.0","id":9,"method":"sampling/createMessage","params":{}}`)
	reply := tr.nextSent(t)
	var got struct {
		ID    int    `json:"id"`
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(reply.Raw, &got); err != nil {
		t.Fatalf("reply %s: %v", reply.Raw, err)
	}
	if got.ID != 9 || got.Error == nil || got.Error.Code != -32601 {
		t.Fatalf("reply = %s; want a -32601 error for id 9", reply.Raw)
	}
}

// TestClient_CancelledCallNotifiesServer: when the user interrupts a turn the
// call's context is cancelled and cove stops waiting - but the server kept
// running the tool (a long search, a build, a browser session) to completion.
// The spec's notifications/cancelled tells it to stop.
func TestClient_CancelledCallNotifiesServer(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	startLoop(c)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Call(ctx, "tools/call", map[string]any{"name": "slow"}, nil) }()
	req := tr.nextSent(t)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v; want context.Canceled", err)
	}

	notif := tr.nextSent(t)
	if notif.Method != "notifications/cancelled" || notif.ID != nil {
		t.Fatalf("after cancelling, the client sent %s; want a notifications/cancelled notification", notif.Raw)
	}
	var body struct {
		Params struct {
			RequestID int `json:"requestId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(notif.Raw, &body); err != nil || body.Params.RequestID != *req.ID {
		t.Fatalf("cancellation %s does not name request %d", notif.Raw, *req.ID)
	}
}

// TestClient_ListToolsFollowsPagination: tools/list is paginated. Servers with
// many tools return the first page plus a nextCursor; reading only that page
// silently hid the rest of the server's tools from the model.
func TestClient_ListToolsFollowsPagination(t *testing.T) {
	tr := newScriptedTransport()
	c := NewClient(tr)
	c.serverCaps.Tools = &ToolsCaps{}
	startLoop(c)
	defer c.Close()

	type out struct {
		tools []Tool
		err   error
	}
	done := make(chan out, 1)
	go func() {
		tools, err := c.ListTools(context.Background())
		done <- out{tools, err}
	}()

	first := tr.nextSent(t)
	tr.incoming <- respJSON(*first.ID, `{"tools":[{"name":"a","inputSchema":{"type":"object"}}],"nextCursor":"page-2"}`)

	second := tr.nextSent(t)
	if second.Method != "tools/list" || !strings.Contains(string(second.Raw), `"cursor":"page-2"`) {
		t.Fatalf("second request = %s; want tools/list with cursor page-2", second.Raw)
	}
	tr.incoming <- respJSON(*second.ID, `{"tools":[{"name":"b","inputSchema":{"type":"object"}}]}`)

	got := <-done
	if got.err != nil {
		t.Fatalf("ListTools: %v", got.err)
	}
	if len(got.tools) != 2 || got.tools[0].Name != "a" || got.tools[1].Name != "b" {
		t.Fatalf("ListTools = %+v; want both pages [a b]", got.tools)
	}
}
