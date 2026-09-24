package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// namedTool is a tool with a real input schema.
type namedTool struct {
	name     string
	readOnly bool
	calls    int
}

func (t *namedTool) Def() tool.Def {
	return tool.Def{
		Name:        t.name,
		Description: t.name + " tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file to read"}},"required":["path"]}`),
		IsReadOnly:  t.readOnly,
	}
}
func (t *namedTool) Validate(tool.Input) string { return "" }
func (t *namedTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Allow}
}
func (t *namedTool) Call(context.Context, tool.Input, tool.Context) (tool.Result, error) {
	t.calls++
	return tool.Result{Data: "ok"}, nil
}

func toolNames(defs []api.ToolDef) []string {
	var out []string
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// The sub-agent used to send every tool as {"type":"object"}, so the model saw
// names and descriptions but no parameters and had to guess argument names.
func TestSubAgentSendsTheToolsRealSchema(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "done"}}}
	sa := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{&namedTool{name: "read", readOnly: true}}})
	sa.Run(context.Background(), "task", "sys")

	if len(prov.lastReq.Tools) != 1 {
		t.Fatalf("tools sent: %v", toolNames(prov.lastReq.Tools))
	}
	props, _ := prov.lastReq.Tools[0].InputSchema["properties"].(map[string]any)
	if _, ok := props["path"]; !ok {
		t.Fatalf("schema sent = %v, want the path parameter", prov.lastReq.Tools[0].InputSchema)
	}
	if req, _ := prov.lastReq.Tools[0].InputSchema["required"].([]any); len(req) != 1 || req[0] != "path" {
		t.Fatalf("required = %v, want [path]", prov.lastReq.Tools[0].InputSchema["required"])
	}
}

// A sub-agent must not be able to spawn more sub-agents or rewrite the plan
// that the parent's executor is running.
func TestSubAgentCannotReachRecursiveOrPlanStateTools(t *testing.T) {
	var tools []tool.Tool
	for _, n := range []string{"read", "agent", "execute_plan", "team_create", "todowrite", "task_update"} {
		tools = append(tools, &namedTool{name: n, readOnly: true})
	}
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "done"}}}
	NewSubAgent(Config{Provider: prov, Model: "m", Tools: tools}).Run(context.Background(), "task", "sys")

	if got := toolNames(prov.lastReq.Tools); len(got) != 1 || got[0] != "read" {
		t.Fatalf("sub-agent was offered %v, want only [read]", got)
	}
}

// The same tool call over and over is a loop; the sub-agent has to stop
// instead of burning all of its iterations on it.
func TestSubAgentStopsOnRepeatedIdenticalToolCalls(t *testing.T) {
	read := &namedTool{name: "read", readOnly: true}
	var responses []*api.ChatResponse
	for i := 0; i < 20; i++ {
		responses = append(responses, &api.ChatResponse{ToolCalls: []api.ToolCall{
			{ID: "x", Name: "read", Input: map[string]any{"path": "a.go"}},
		}})
	}
	prov := &fakeProvider{responses: responses}
	res := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{read}}).Run(context.Background(), "task", "sys")

	if res.Success || !strings.Contains(res.Error, "loop") {
		t.Fatalf("result = %+v, want a loop failure", res)
	}
	if read.calls != 2 {
		t.Fatalf("repeated call executed %d times, want 2 (the third identical batch is refused)", read.calls)
	}
}

// When an executor is supplied, tool calls go through it — the engine's full
// pipeline (safety scan, hooks, guardrails, permission, output limits) —
// instead of the sub-agent calling the tool directly.
func TestSubAgentRoutesToolCallsThroughTheExecutor(t *testing.T) {
	read := &namedTool{name: "read", readOnly: true}
	prov := &fakeProvider{responses: []*api.ChatResponse{
		{ToolCalls: []api.ToolCall{{ID: "c1", Name: "read", Input: map[string]any{"path": "a.go"}}}},
		{Content: "done"},
	}}
	var executed []string
	sa := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{read},
		Executor: func(ctx context.Context, tc api.ToolCall) string {
			executed = append(executed, tc.Name)
			return "from executor"
		}})
	sa.Run(context.Background(), "task", "sys")

	if len(executed) != 1 || read.calls != 0 {
		t.Fatalf("executor saw %v, direct calls %d; want the executor to handle the call", executed, read.calls)
	}
	var sawResult bool
	for _, m := range prov.lastReq.Messages {
		if m.Role == "tool" && m.Content == "from executor" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("executor output never reached the model: %+v", prov.lastReq.Messages)
	}
}

// Sub-agent tool results were clipped to 4000 bytes on top of the engine's own
// limit, so a sub-agent could see little more than the first page of a file.
func TestSubAgentKeepsSubstantialToolOutput(t *testing.T) {
	body := strings.Repeat("z", 30000)
	prov := &fakeProvider{responses: []*api.ChatResponse{
		{ToolCalls: []api.ToolCall{{ID: "c1", Name: "read", Input: map[string]any{"path": "a"}}}},
		{Content: "done"},
	}}
	sa := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{&namedTool{name: "read", readOnly: true}},
		Executor: func(context.Context, api.ToolCall) string { return body }})
	sa.Run(context.Background(), "task", "sys")
	for _, m := range prov.lastReq.Messages {
		if m.Role == "tool" && len(m.Content) < len(body) {
			t.Fatalf("sub-agent tool result clipped to %d of %d bytes", len(m.Content), len(body))
		}
	}
}

func TestSubAgentStopsWhenBudgetIsExceeded(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{
		{ToolCalls: []api.ToolCall{{ID: "c1", Name: "read", Input: map[string]any{"path": "a"}}}},
		{Content: "never reached"},
	}}
	spent := false
	prov.onCall = func() { spent = true } // the first call exhausts the budget
	sa := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{&namedTool{name: "read", readOnly: true}},
		BudgetExceeded: func() bool { return spent }})

	res := sa.Run(context.Background(), "task", "sys")
	if res.Success || !strings.Contains(res.Error, "budget") {
		t.Fatalf("result = %+v, want a budget failure", res)
	}
	if prov.seen != 1 {
		t.Fatalf("made %d model calls, want 1 (no call after the budget ran out)", prov.seen)
	}
}

func TestSubAgentReportsTokenUsage(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{
		{ToolCalls: []api.ToolCall{{ID: "c1", Name: "read", Input: map[string]any{"path": "a"}}}, InputTokens: 10, OutputTokens: 2},
		{Content: "done", InputTokens: 30, OutputTokens: 5},
	}}
	res := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{&namedTool{name: "read", readOnly: true}}}).
		Run(context.Background(), "task", "sys")
	if res.InputTokens != 40 || res.OutputTokens != 7 {
		t.Fatalf("usage = %d in / %d out, want 40 / 7", res.InputTokens, res.OutputTokens)
	}
}

// The provider and model are resolved when a task starts, so a provider or
// model switch after the delegator was built reaches later sub-agents.
func TestDelegatorResolvesProviderWhenTheTaskStarts(t *testing.T) {
	old := &fakeProvider{}
	d := NewDelegator(old, "old-model", nil)
	fresh := &fakeProvider{responses: []*api.ChatResponse{{Content: "done"}}}
	d.SetProviderSource(func() (api.Provider, string) { return fresh, "new-model" })

	d.Delegate(context.Background(), "t1", "task", "sys")
	if old.seen != 0 || fresh.seen != 1 {
		t.Fatalf("calls old=%d fresh=%d, want the current provider to serve", old.seen, fresh.seen)
	}
	if fresh.lastReq.Model != "new-model" {
		t.Fatalf("model = %q, want new-model", fresh.lastReq.Model)
	}
}

func TestDelegateReadOnlyOffersOnlyReadOnlyTools(t *testing.T) {
	prov := &fakeProvider{responses: []*api.ChatResponse{{Content: "done"}}}
	d := NewDelegator(prov, "m", []tool.Tool{&namedTool{name: "read", readOnly: true}, &namedTool{name: "write"}})
	d.DelegateWith(context.Background(), "t1", "look around", "sys", Options{ReadOnly: true})
	if got := toolNames(prov.lastReq.Tools); len(got) != 1 || got[0] != "read" {
		t.Fatalf("read-only sub-agent was offered %v", got)
	}
}
