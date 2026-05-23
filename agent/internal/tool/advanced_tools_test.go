package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentgo/internal/mcp"
	"github.com/agentgo/internal/skills"
)

func TestPlanModeTool(t *testing.T) {
	tp := NewPlanModeTool()
	if tp.Def().Name != "plan_mode" {
		t.Errorf("expected name 'plan_mode', got %q", tp.Def().Name)
	}
	if !tp.Def().IsReadOnly {
		t.Error("plan_mode should be read-only")
	}

	rt := &Runtime{}
	ctx := Context{Runtime: rt}
	result, _ := tp.Call(context.Background(), Input{"reason": "testing"}, ctx)
	if result.IsError {
		t.Errorf("plan_mode failed: %s", result.Data)
	}
	if !rt.PlanMode {
		t.Error("Runtime.PlanMode should be true")
	}

	perm := tp.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}
}

func TestExitPlanModeTool(t *testing.T) {
	tep := NewExitPlanModeTool()
	if tep.Def().Name != "exit_plan_mode" {
		t.Errorf("expected name 'exit_plan_mode', got %q", tep.Def().Name)
	}

	rt := &Runtime{PlanMode: true}
	ctx := Context{Runtime: rt}
	result, _ := tep.Call(context.Background(), Input{"summary": "done testing"}, ctx)
	if result.IsError {
		t.Errorf("exit_plan_mode failed: %s", result.Data)
	}
	if rt.PlanMode {
		t.Error("Runtime.PlanMode should be false")
	}

	perm := tep.CheckPermissions(nil, Context{})
	if perm.Decision != Ask {
		t.Errorf("expected Ask, got %v", perm.Decision)
	}
}

func TestSleepTool(t *testing.T) {
	ts := NewSleepTool()
	if ts.Def().Name != "sleep" {
		t.Errorf("expected name 'sleep', got %q", ts.Def().Name)
	}

	// Test with 0 seconds should clamp to 1
	result, _ := ts.Call(context.Background(), Input{"seconds": float64(0)}, Context{})
	if result.IsError {
		t.Errorf("sleep failed: %s", result.Data)
	}

	// Large sleeps should remain interruptible instead of stalling the test suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result, _ = ts.Call(ctx, Input{"seconds": float64(500)}, Context{})
	if !result.IsError || result.Data != "Sleep interrupted" {
		t.Errorf("expected interrupted sleep, got %+v", result)
	}

	perm := ts.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}
}

type fakeLSPRunner struct {
	output string
	err    error
}

func (f *fakeLSPRunner) Run(ctx context.Context, action string, filePath string, input Input) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.output, nil
}

type fakeRuntimeSkillManager struct {
	skills map[string]skills.Skill
}

func (f *fakeRuntimeSkillManager) Get(name string) (skills.Skill, bool) {
	skill, ok := f.skills[name]
	return skill, ok
}

func (f *fakeRuntimeSkillManager) All() []skills.Skill {
	all := make([]skills.Skill, 0, len(f.skills))
	for _, skill := range f.skills {
		all = append(all, skill)
	}
	return all
}

type fakeMCPToolPool struct {
	servers      []*mcp.ManagedServer
	tools        []mcp.ToolRef
	callResult   *mcp.CallToolResult
	callErr      error
	readResult   *mcp.ReadResourceResult
	readErr      error
}

func (f *fakeMCPToolPool) AllTools() []mcp.ToolRef { return f.tools }
func (f *fakeMCPToolPool) AllServers() []*mcp.ManagedServer { return f.servers }
func (f *fakeMCPToolPool) CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	return f.callResult, f.callErr
}
func (f *fakeMCPToolPool) ReadResource(ctx context.Context, serverName, uri string) (*mcp.ReadResourceResult, error) {
	return f.readResult, f.readErr
}

func TestBriefTool(t *testing.T) {
	tb := NewBriefTool()
	if tb.Def().Name != "brief" {
		t.Errorf("expected name 'brief', got %q", tb.Def().Name)
	}
	result, _ := tb.Call(context.Background(), Input{"what": "test"}, Context{Runtime: &Runtime{Tasks: map[string]*TaskRecord{"task-1": {
		ID:          "task-1",
		Title:       "demo task",
		Description: "test desc",
		Status:      "completed",
		Output:      "done",
	}}}})
	if result.IsError {
		t.Errorf("brief failed: %s", result.Data)
	}
	if strings.Contains(result.Data, "provided by the assistant") {
		t.Fatalf("brief should return runtime-derived summary, got %q", result.Data)
	}
	if !strings.Contains(result.Data, "demo task") || !strings.Contains(result.Data, "completed") {
		t.Fatalf("expected task details in brief summary, got %q", result.Data)
	}

	perm := tb.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}
}

func TestSkillTool(t *testing.T) {
	tsk := NewSkillTool()
	if tsk.Def().Name != "skill" {
		t.Errorf("expected name 'skill', got %q", tsk.Def().Name)
	}

	// Without runtime
	result, _ := tsk.Call(context.Background(), Input{"name": "test"}, Context{})
	if result.IsError {
		t.Errorf("skill without runtime failed: %s", result.Data)
	}

	// With runtime and skill prompts
	rt := &Runtime{SkillPrompts: map[string]string{"test": "do something"}}
	ctx := Context{Runtime: rt}
	result, _ = tsk.Call(context.Background(), Input{"name": "test"}, ctx)
	if result.IsError {
		t.Errorf("skill with runtime failed: %s", result.Data)
	}

	perm := tsk.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}
}

func TestSkillToolFallsBackToRuntimeSkillManager(t *testing.T) {
	tsk := NewSkillTool()
	rt := &Runtime{SkillManager: &fakeRuntimeSkillManager{skills: map[string]skills.Skill{
		"debug": {Name: "debug", Prompt: "DEBUG WORKFLOW"},
	}}}

	result, _ := tsk.Call(context.Background(), Input{"name": "debug"}, Context{Runtime: rt})
	if result.IsError {
		t.Fatalf("skill with runtime skill manager failed: %s", result.Data)
	}
	if !strings.Contains(result.Data, "DEBUG WORKFLOW") {
		t.Fatalf("expected runtime skill manager prompt, got %q", result.Data)
	}
	if strings.Contains(result.Data, "Available skills") || strings.Contains(result.Data, "No runtime skill registry available") {
		t.Fatalf("expected concrete prompt instead of fallback response, got %q", result.Data)
	}
}

func TestTaskCreateListUpdate(t *testing.T) {
	rt := &Runtime{Tasks: make(map[string]*TaskRecord)}
	ctx := Context{Runtime: rt}

	// Create
	tc := NewTaskCreateTool()
	result, _ := tc.Call(context.Background(), Input{"title": "test", "description": "test desc"}, ctx)
	if result.IsError {
		t.Errorf("task create failed: %s", result.Data)
	}

	// List
	tl := NewTaskListTool()
	result, _ = tl.Call(context.Background(), nil, ctx)
	if result.IsError {
		t.Errorf("task list failed: %s", result.Data)
	}

	// Update
	tu := NewTaskUpdateTool()
	var taskID string
	for id := range rt.Tasks {
		taskID = id
		break
	}
	result, _ = tu.Call(context.Background(), Input{"taskId": taskID, "status": "completed", "output": "done"}, ctx)
	if result.IsError {
		t.Errorf("task update failed: %s", result.Data)
	}
	if rt.Tasks[taskID].Status != "completed" {
		t.Errorf("expected completed, got %s", rt.Tasks[taskID].Status)
	}
	if rt.Tasks[taskID].Output != "done" {
		t.Errorf("expected output 'done', got %s", rt.Tasks[taskID].Output)
	}

	// Empty task list
	rtEmpty := &Runtime{Tasks: make(map[string]*TaskRecord)}
	ctxEmpty := Context{Runtime: rtEmpty}
	result, _ = tl.Call(context.Background(), nil, ctxEmpty)
	if result.IsError {
		t.Errorf("empty task list failed: %s", result.Data)
	}
}

func TestTaskUpdateNotFound(t *testing.T) {
	rt := &Runtime{Tasks: make(map[string]*TaskRecord)}
	ctx := Context{Runtime: rt}

	tu := NewTaskUpdateTool()
	result, _ := tu.Call(context.Background(), Input{"taskId": "nonexistent", "status": "completed"}, ctx)
	if result.IsError {
		t.Errorf("task update for non-existent should not error: %s", result.Data)
	}
}

func TestAgentTool(t *testing.T) {
	ta := NewAgentTool()
	if ta.Def().Name != "agent" {
		t.Errorf("expected name 'agent', got %q", ta.Def().Name)
	}

	// Without agent runner
	result, _ := ta.Call(context.Background(), Input{"type": "general", "prompt": "do something"}, Context{})
	if result.IsError {
		t.Errorf("agent without runner failed: %s", result.Data)
	}

	perm := ta.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}
}

func TestWorktreeTools(t *testing.T) {
	// Exit without active worktree
	tew := NewExitWorktreeTool()
	rt := &Runtime{}
	ctx := Context{Runtime: rt}
	result, _ := tew.Call(context.Background(), nil, ctx)
	if !result.IsError {
		t.Errorf("expected error when no active worktree")
	}

	// Enter worktree permissions
	tent := NewEnterWorktreeTool()
	perm := tent.CheckPermissions(nil, Context{})
	if perm.Decision != Ask {
		t.Errorf("expected Ask, got %v", perm.Decision)
	}

	// Exit worktree permissions
	perm = tew.CheckPermissions(nil, Context{})
	if perm.Decision != Ask {
		t.Errorf("expected Ask, got %v", perm.Decision)
	}
}

func TestCronTool(t *testing.T) {
	tc := NewCronTool()
	if tc.Def().Name != "cron" {
		t.Errorf("expected name 'cron', got %q", tc.Def().Name)
	}
	perm := tc.CheckPermissions(nil, Context{})
	if perm.Decision != Ask {
		t.Errorf("expected Ask, got %v", perm.Decision)
	}

	rt := &Runtime{Tasks: make(map[string]*TaskRecord)}
	result, _ := tc.Call(context.Background(), Input{"schedule": "0 9 * * *", "task": "daily sync"}, Context{Runtime: rt})
	if result.IsError {
		t.Fatalf("cron failed: %s", result.Data)
	}
	if strings.Contains(result.Data, "Cron scheduled:") {
		t.Fatalf("cron should return runtime-backed record, got %q", result.Data)
	}
	if len(rt.Tasks) == 0 {
		t.Fatal("cron should create a runtime task record")
	}
}

func TestLSPTool(t *testing.T) {
	tl := NewLSPTool()
	if tl.Def().Name != "lsp" {
		t.Errorf("expected name 'lsp', got %q", tl.Def().Name)
	}

	result, _ := tl.Call(context.Background(), Input{"action": "diagnostics", "filePath": "test.go"}, Context{})
	if !result.IsError {
		t.Fatalf("lsp without backend should surface an error, got %q", result.Data)
	}

	rt := &Runtime{LSPRunner: &fakeLSPRunner{output: "diagnostics: clean"}}
	result, _ = tl.Call(context.Background(), Input{"action": "diagnostics", "filePath": "test.go"}, Context{Runtime: rt})
	if result.IsError {
		t.Fatalf("lsp with backend should succeed, got %q", result.Data)
	}
	if !strings.Contains(result.Data, "diagnostics: clean") {
		t.Fatalf("expected lsp runner output, got %q", result.Data)
	}
}

func TestTeamTools(t *testing.T) {
	// Team Create permissions
	tc := NewTeamCreateTool()
	if tc.Def().Name != "team_create" {
		t.Errorf("expected name 'team_create', got %q", tc.Def().Name)
	}
	perm := tc.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}

	rt := &Runtime{Tasks: make(map[string]*TaskRecord)}
	result, _ := tc.Call(context.Background(), Input{"name": "alpha", "members": []any{
		map[string]any{"agent": "review", "task": "review backend"},
		map[string]any{"agent": "test", "task": "run tests"},
	}}, Context{Runtime: rt})
	if result.IsError {
		t.Fatalf("team create failed: %s", result.Data)
	}
	if len(rt.Tasks) != 2 {
		t.Fatalf("expected team members mirrored into runtime tasks, got %d", len(rt.Tasks))
	}

	// Team Delete permissions
	td := NewTeamDeleteTool()
	if td.Def().Name != "team_delete" {
		t.Errorf("expected name 'team_delete', got %q", td.Def().Name)
	}
	perm = td.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}
	result, _ = td.Call(context.Background(), Input{"name": "alpha"}, Context{Runtime: rt})
	if result.IsError {
		t.Fatalf("team delete failed: %s", result.Data)
	}
	if strings.Contains(result.Data, "deleted.") {
		t.Fatalf("team delete should describe runtime cleanup, got %q", result.Data)
	}
}

func TestSendMessageTool(t *testing.T) {
	tsm := NewSendMessageTool()
	if tsm.Def().Name != "send_message" {
		t.Errorf("expected name 'send_message', got %q", tsm.Def().Name)
	}
	perm := tsm.CheckPermissions(nil, Context{})
	if perm.Decision != Allow {
		t.Errorf("expected Allow, got %v", perm.Decision)
	}

	rt := &Runtime{Tasks: map[string]*TaskRecord{"task-1": {ID: "task-1", Title: "receiver", Status: "running"}}}
	result, _ := tsm.Call(context.Background(), Input{"to": "task-1", "message": "hello world"}, Context{Runtime: rt})
	if result.IsError {
		t.Fatalf("send_message failed: %s", result.Data)
	}
	if strings.Contains(result.Data, "Message to [task-1]") {
		t.Fatalf("send_message should update runtime state, got %q", result.Data)
	}
	if !strings.Contains(rt.Tasks["task-1"].Output, "hello world") {
		t.Fatalf("expected runtime task output to include message, got %q", rt.Tasks["task-1"].Output)
	}
}


func TestMCPResourceTools(t *testing.T) {
	pool := &fakeMCPToolPool{
		servers: []*mcp.ManagedServer{{
			Name: "demo",
			Tools: []mcp.Tool{{Name: "search", Description: "Search docs"}},
			Resources: []mcp.Resource{{URI: "file:///README.md", Name: "README", Description: "Project readme"}},
		}},
		tools: []mcp.ToolRef{{Server: "demo", Tool: mcp.Tool{Name: "search", Description: "Search docs"}}},
		readResult: &mcp.ReadResourceResult{Contents: []mcp.ContentBlock{{Type: "text", Text: "hello from tool resource"}}},
	}

	listTool := NewListMCPResourcesTool(pool)
	listResult, _ := listTool.Call(context.Background(), Input{}, Context{})
	if listResult.IsError {
		t.Fatalf("list resources failed: %s", listResult.Data)
	}
	if !strings.Contains(listResult.Data, "resource: file:///README.md") {
		t.Fatalf("expected resource listing, got %q", listResult.Data)
	}

	readTool := NewReadMCPResourceTool(pool)
	readResult, _ := readTool.Call(context.Background(), Input{"serverName": "demo", "uri": "file:///README.md"}, Context{})
	if readResult.IsError {
		t.Fatalf("read resource failed: %s", readResult.Data)
	}
	if !strings.Contains(readResult.Data, "hello from tool resource") {
		t.Fatalf("expected resource contents, got %q", readResult.Data)
	}
}
