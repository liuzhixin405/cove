package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// fakeProvider replays one scripted response per Chat call.
type fakeProvider struct {
	responses []*api.ChatResponse
	seen      int
	lastReq   api.ChatRequest
}

func (p *fakeProvider) Name() string        { return "fake" }
func (p *fakeProvider) DisplayName() string { return "Fake" }
func (p *fakeProvider) Validate() error     { return nil }

func (p *fakeProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.lastReq = req
	if p.seen >= len(p.responses) {
		return &api.ChatResponse{Content: "done"}, nil
	}
	r := p.responses[p.seen]
	p.seen++
	return r, nil
}

func (p *fakeProvider) ChatStream(ctx context.Context, req api.ChatRequest, h api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

// probeTool records how it was invoked, so the test can see the tool.Context a
// sub-agent builds for its tool calls.
type probeTool struct {
	readOnly bool
	calls    int
	lastCtx  tool.Context
	lastIn   tool.Input
}

func (t *probeTool) Def() tool.Def {
	return tool.Def{
		Name:              "write",
		Description:       "probe",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"filePath":{"type":"string"}}}`),
		IsReadOnly:        t.readOnly,
		IsConcurrencySafe: false,
		UserFacingName:    "Write",
	}
}

func (t *probeTool) Validate(input tool.Input) string { return "" }

func (t *probeTool) CheckPermissions(input tool.Input, tctx tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Ask, Reason: "writes a file"}
}

func (t *probeTool) Call(ctx context.Context, input tool.Input, tctx tool.Context) (tool.Result, error) {
	t.calls++
	t.lastCtx = tctx
	t.lastIn = input
	return tool.Result{Data: "written"}, nil
}

func writeCall() *api.ChatResponse {
	return &api.ChatResponse{ToolCalls: []api.ToolCall{
		{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}},
	}}
}

// A sub-agent used to call every tool directly, with PermissionMode hardcoded
// to "auto" and no permission check at all. A user who approved a plan in
// default mode then had sub-agents writing files with no further gate.
func TestSubAgentAsksTheGateBeforeRunningATool(t *testing.T) {
	probe := &probeTool{}
	prov := &fakeProvider{responses: []*api.ChatResponse{writeCall(), {Content: "ok"}}}

	var asked []string
	sa := NewSubAgent(Config{
		Provider: prov,
		Model:    "m",
		Tools:    []tool.Tool{probe},
		Authorize: func(ctx context.Context, tc api.ToolCall) error {
			asked = append(asked, tc.Name)
			return errors.New("permission denied for write: user rejected")
		},
	})

	res := sa.Run(context.Background(), "write a.go", "sys")
	if res == nil {
		t.Fatal("Run returned nil")
	}
	if len(asked) != 1 || asked[0] != "write" {
		t.Fatalf("the gate saw %v, want exactly one call for \"write\"", asked)
	}
	if probe.calls != 0 {
		t.Errorf("a tool the gate denied was executed anyway (%d calls)", probe.calls)
	}

	// The model has to be told, or it reports success for a write that never
	// happened.
	var told bool
	for _, m := range prov.lastReq.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "permission denied") {
			told = true
		}
	}
	if !told {
		t.Errorf("the denial never reached the model; messages were %+v", prov.lastReq.Messages)
	}
}

func TestSubAgentRunsAToolTheGateAllows(t *testing.T) {
	probe := &probeTool{}
	prov := &fakeProvider{responses: []*api.ChatResponse{writeCall(), {Content: "ok"}}}

	sa := NewSubAgent(Config{
		Provider:  prov,
		Model:     "m",
		Tools:     []tool.Tool{probe},
		Authorize: func(ctx context.Context, tc api.ToolCall) error { return nil },
	})

	res := sa.Run(context.Background(), "write a.go", "sys")
	if res == nil || !res.Success {
		t.Fatalf("Run = %+v, want success", res)
	}
	if probe.calls != 1 {
		t.Fatalf("allowed tool ran %d times, want 1", probe.calls)
	}
	if got := probe.lastIn["filePath"]; got != "a.go" {
		t.Errorf("tool input was %v, want filePath=a.go", probe.lastIn)
	}
}

// Tools resolve relative paths against Context.Cwd. The sub-agent left it empty,
// so a relative write from a sub-agent resolved against the process working
// directory instead of the project the user is in.
func TestSubAgentToolCallsCarryCwdAndRealPermissionMode(t *testing.T) {
	probe := &probeTool{}
	prov := &fakeProvider{responses: []*api.ChatResponse{writeCall(), {Content: "ok"}}}

	sa := NewSubAgent(Config{
		Provider:       prov,
		Model:          "m",
		Tools:          []tool.Tool{probe},
		Cwd:            "/work/project",
		PermissionMode: "default",
		Authorize:      func(ctx context.Context, tc api.ToolCall) error { return nil },
	})

	sa.Run(context.Background(), "write a.go", "sys")

	if probe.lastCtx.Cwd != "/work/project" {
		t.Errorf("tool saw Cwd %q, want /work/project", probe.lastCtx.Cwd)
	}
	if probe.lastCtx.PermissionMode != "default" {
		t.Errorf("tool saw PermissionMode %q, want the session's real mode", probe.lastCtx.PermissionMode)
	}
}

// A gate that was never wired must not leave the door open. Read-only tools
// stay usable so a sub-agent is still worth spawning; anything that can mutate
// the workspace is refused.
func TestSubAgentWithoutAGateOnlyRunsReadOnlyTools(t *testing.T) {
	readOnly := &probeTool{readOnly: true}
	write := &probeTool{readOnly: false}
	// Both are named "write" in Def(); give the read-only one its own name so
	// the registry keeps both distinct.
	readOnlyTool := &namedProbe{probeTool: readOnly, name: "read"}

	prov := &fakeProvider{responses: []*api.ChatResponse{
		{ToolCalls: []api.ToolCall{
			{ID: "tc1", Name: "read", Input: map[string]any{"filePath": "a.go"}},
			{ID: "tc2", Name: "write", Input: map[string]any{"filePath": "a.go"}},
		}},
		{Content: "ok"},
	}}

	sa := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{readOnlyTool, write}})
	sa.Run(context.Background(), "do it", "sys")

	if readOnly.calls != 1 {
		t.Errorf("read-only tool ran %d times, want 1", readOnly.calls)
	}
	if write.calls != 0 {
		t.Errorf("a mutating tool ran with no gate configured (%d calls)", write.calls)
	}
}

// namedProbe lets a probeTool register under a different name.
type namedProbe struct {
	*probeTool
	name string
}

func (n *namedProbe) Def() tool.Def {
	d := n.probeTool.Def()
	d.Name = n.name
	return d
}
