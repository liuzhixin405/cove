package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/tool"
)

// ===========================================================================
// Mock Provider 鈥?simulates API responses for integration testing
// ===========================================================================

type mockProvider struct {
	mu         sync.Mutex
	responses  []mockResponse // queue of responses to return
	callCount  int
	lastReq    api.ChatRequest
	streamMode bool
}

type mockResponse struct {
	content   string
	toolCalls []api.ToolCall
	err       error
	delay     time.Duration // simulate API latency
}

func (m *mockProvider) Name() string        { return "mock" }
func (m *mockProvider) DisplayName() string { return "Mock Provider" }
func (m *mockProvider) Validate() error     { return nil }

func (m *mockProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	m.mu.Lock()
	idx := m.callCount
	m.callCount++
	m.lastReq = req
	m.mu.Unlock()

	if idx >= len(m.responses) {
		return &api.ChatResponse{Content: "done", StopReason: "stop"}, nil
	}
	resp := m.responses[idx]
	if resp.delay > 0 {
		select {
		case <-time.After(resp.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if resp.err != nil {
		return nil, resp.err
	}
	return &api.ChatResponse{
		Content:    resp.content,
		ToolCalls:  resp.toolCalls,
		StopReason: "stop",
	}, nil
}

func (m *mockProvider) ChatStream(ctx context.Context, req api.ChatRequest, handler api.StreamHandler) (*api.ChatResponse, error) {
	m.mu.Lock()
	idx := m.callCount
	m.callCount++
	m.lastReq = req
	m.streamMode = true
	m.mu.Unlock()

	if idx >= len(m.responses) {
		if handler != nil {
			handler(api.StreamEvent{Type: "delta", Delta: "done"})
		}
		return &api.ChatResponse{Content: "done", StopReason: "stop"}, nil
	}
	resp := m.responses[idx]
	if resp.delay > 0 {
		select {
		case <-time.After(resp.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if resp.err != nil {
		return nil, resp.err
	}
	if handler != nil && resp.content != "" {
		handler(api.StreamEvent{Type: "delta", Delta: resp.content})
	}
	return &api.ChatResponse{
		Content:    resp.content,
		ToolCalls:  resp.toolCalls,
		StopReason: "stop",
	}, nil
}

// ===========================================================================
// Mock Tool 鈥?controllable tool for testing permission/execution paths
// ===========================================================================

type mockTool struct {
	name      string
	readOnly  bool
	safe      bool
	callCount int
	result    string
	err       error
	panicMsg  string
	delay     time.Duration
}

func (t *mockTool) Def() tool.Def {
	return tool.Def{
		Name:              t.name,
		Description:       "mock tool for testing",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}}}`),
		IsReadOnly:        t.readOnly,
		IsConcurrencySafe: t.safe,
		UserFacingName:    t.name,
	}
}

func (t *mockTool) Validate(input tool.Input) string {
	return "" // always valid
}

func (t *mockTool) CheckPermissions(input tool.Input, tctx tool.Context) tool.PermissionDecision {
	if t.readOnly {
		return tool.PermissionDecision{Decision: tool.Allow}
	}
	return tool.PermissionDecision{Decision: tool.Ask, Reason: "write operation"}
}

func (t *mockTool) Call(ctx context.Context, input tool.Input, tctx tool.Context) (tool.Result, error) {
	t.callCount++
	if t.delay > 0 {
		select {
		case <-time.After(t.delay):
		case <-ctx.Done():
			return tool.Result{Data: "cancelled", IsError: true}, ctx.Err()
		}
	}
	if t.panicMsg != "" {
		panic(t.panicMsg)
	}
	if t.err != nil {
		return tool.Result{Data: t.err.Error(), IsError: true}, nil
	}
	return tool.Result{Data: t.result}, nil
}

// ===========================================================================
// Helper to create a test engine
// ===========================================================================

func newTestEngine(provider *mockProvider, tools ...tool.Tool) *Engine {
	cfg := Config{
		Model:          "test-model",
		PermissionMode: "auto",
		MaxBudget:      100,
		Provider:       api.ProviderConfig{Name: "mock", APIKey: "sk-test"},
	}
	if tools != nil {
		cfg.Tools = tools
	}

	eng, err := New(cfg)
	if err != nil {
		panic(fmt.Sprintf("newTestEngine failed: %v", err))
	}
	// Inject our mock provider
	eng.SetProvider(provider)
	return eng
}

// ===========================================================================
// TEST: Basic message flow 鈥?send message, get response
// ===========================================================================

func TestEngineBasicMessageFlow(t *testing.T) {
	prov := &mockProvider{
		responses: []mockResponse{
			{content: "Hello! How can I help?"},
		},
	}
	eng := newTestEngine(prov)

	ctx := context.Background()
	var output strings.Builder
	reply, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "hi",
	}, func(delta string) {
		output.WriteString(delta)
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply == "" {
		t.Fatal("expected non-empty reply")
	}
	if output.Len() == 0 {
		t.Fatal("expected stream output")
	}
}

// ===========================================================================
// TEST: Context cancellation 鈥?Ctrl+C during API call
// ===========================================================================

func TestEngineContextCancellation(t *testing.T) {
	prov := &mockProvider{
		responses: []mockResponse{
			{content: "slow response", delay: 5 * time.Second},
		},
	}
	eng := newTestEngine(prov)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "do something slow",
	}, nil, nil)

	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected context error, got: %v", err)
	}
}

// ===========================================================================
// TEST: Tool execution 鈥?model requests tool, tool runs, result returned
// ===========================================================================

func TestEngineToolExecution(t *testing.T) {
	readTool := &mockTool{name: "read", readOnly: true, safe: true, result: "file content here"}

	prov := &mockProvider{
		responses: []mockResponse{
			// First response: model requests tool call
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "read", Input: map[string]any{"input": "test.txt"}}}},
			// Second response: model provides final answer
			{content: "I read the file, it contains 'file content here'"},
		},
	}
	eng := newTestEngine(prov, readTool)

	ctx := context.Background()
	reply, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "read test.txt",
	}, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(reply, "file content here") {
		t.Fatalf("expected reply to contain tool result, got: %s", reply)
	}
	if readTool.callCount != 1 {
		t.Fatalf("expected tool called once, got %d", readTool.callCount)
	}
}

func TestSummarizeResultPreservesWritePath(t *testing.T) {
	path := `D:\gitlab\RC_SZ\ccs-backend\CentralizedConfigurationSystem\very\long\nested\path\settings.yaml`
	in := "Wrote 2751 bytes (60 lines) to " + path
	out := summarizeResult(in)

	if !strings.Contains(out, " to "+path) {
		t.Fatalf("expected full path preserved, got %q", out)
	}
}

func TestSummarizeResultTruncatesNonPathLongLine(t *testing.T) {
	in := strings.Repeat("x", 120)
	out := summarizeResult(in)
	if len(out) > 80 {
		t.Fatalf("expected truncated output <= 80 chars, got %d", len(out))
	}
	if !strings.HasSuffix(out, "...") {
		t.Fatalf("expected ellipsis for truncated output, got %q", out)
	}
}

func TestSummarizeResultPreservesFilePrefixPath(t *testing.T) {
	path := `D:\gitlab\RC_SZ\ccs-backend\CentralizedConfigurationSystem.Application\Modules\MT5\Symbols\Handlers\TransferNewSymbolToServerHandler.cs`
	in := "File: " + path
	out := summarizeResult(in)
	if !strings.Contains(out, path) {
		t.Fatalf("expected file path preserved, got %q", out)
	}
}

func TestSummarizeResultPreservesFileNotFoundPath(t *testing.T) {
	path := `D:\gitlab\RC_SZ\ccs-backend\CentralizedConfigurationSystem.Api\Controllers\MissingController.cs`
	in := "Error: file not found: " + path
	out := summarizeResult(in)
	if !strings.Contains(out, path) {
		t.Fatalf("expected missing file path preserved, got %q", out)
	}
}

func TestSummarizeResultPreservesGlobPathLine(t *testing.T) {
	path := `CentralizedConfigurationSystem.Application\Modules\MT5\Symbols\Handlers\TransferNewSymbolToServerHandler.cs`
	in := path + " matched by glob"
	out := summarizeResult(in)
	if !strings.Contains(out, path) {
		t.Fatalf("expected glob path preserved, got %q", out)
	}
}

// ===========================================================================
// TEST: Permission denied 鈥?tool requires permission, user denies
// ===========================================================================

func TestEnginePermissionDenied(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "written"}

	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "write", Input: map[string]any{"input": "data"}}}},
			{content: "OK, I won't write the file."},
		},
	}
	eng := newTestEngine(prov, writeTool)
	// Override permission mode to require asking
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)
	// User always denies
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		return false
	}

	ctx := context.Background()
	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "write something",
	}, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Tool should NOT have been called
	if writeTool.callCount != 0 {
		t.Fatalf("expected tool NOT called (permission denied), got %d calls", writeTool.callCount)
	}
}

// ===========================================================================
// TEST: Permission prompt not set 鈥?should not hang, should deny gracefully
// ===========================================================================

func TestEnginePermissionPromptNil(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "written"}

	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "write", Input: map[string]any{"input": "data"}}}},
			{content: "Permission was denied."},
		},
	}
	eng := newTestEngine(prov, writeTool)
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)
	eng.PermissionPrompt = nil // NOT SET 鈥?this should not hang

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "write file",
	}, nil, nil)

	if err != nil {
		// Should not timeout 鈥?if it does, there's a hang
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("HANG DETECTED: engine blocked waiting for permission prompt that was never set")
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if writeTool.callCount != 0 {
		t.Fatal("tool should not have been called without permission")
	}
}

// ===========================================================================
// TEST: Tool panic recovery 鈥?goroutine panic should not crash process
// ===========================================================================

func TestEngineToolPanicRecovery(t *testing.T) {
	panicTool := &mockTool{name: "read", readOnly: true, safe: true, panicMsg: "unexpected nil pointer"}
	normalTool := &mockTool{name: "list", readOnly: true, safe: true, result: "a.txt\nb.txt"}

	prov := &mockProvider{
		responses: []mockResponse{
			// Model requests two tools in parallel 鈥?one panics
			{toolCalls: []api.ToolCall{
				{ID: "tc1", Name: "read", Input: map[string]any{"input": "crash.txt"}},
				{ID: "tc2", Name: "list", Input: map[string]any{"input": "."}},
			}},
			{content: "The read failed but list worked."},
		},
	}
	eng := newTestEngine(prov, panicTool, normalTool)

	ctx := context.Background()
	reply, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "read and list",
	}, nil, nil)

	if err != nil {
		t.Fatalf("engine should recover from panic, got error: %v", err)
	}
	// The normal tool should still have executed
	if normalTool.callCount == 0 {
		t.Fatal("non-panicking tool should still execute")
	}
	_ = reply // reply contains the model's response to the error
}

// ===========================================================================
// TEST: API error 鈥?should return error, not hang
// ===========================================================================

func TestEngineAPIError(t *testing.T) {
	prov := &mockProvider{
		responses: []mockResponse{
			{err: fmt.Errorf("API error 500: internal server error")},
		},
	}
	eng := newTestEngine(prov)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "hello",
	}, nil, nil)

	if err == nil {
		t.Fatal("expected error from API failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("HANG DETECTED: engine blocked on API error instead of returning")
	}
}

// ===========================================================================
// TEST: Multiple iterations 鈥?model calls tools across multiple turns
// ===========================================================================

func TestEngineMultipleIterations(t *testing.T) {
	readTool := &mockTool{name: "read", readOnly: true, safe: true, result: "content"}

	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "read", Input: map[string]any{"input": "a.txt"}}}},
			{toolCalls: []api.ToolCall{{ID: "tc2", Name: "read", Input: map[string]any{"input": "b.txt"}}}},
			{toolCalls: []api.ToolCall{{ID: "tc3", Name: "read", Input: map[string]any{"input": "c.txt"}}}},
			{content: "Done reading all three files."},
		},
	}
	eng := newTestEngine(prov, readTool)

	ctx := context.Background()
	reply, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "read a, b, c",
	}, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if readTool.callCount != 3 {
		t.Fatalf("expected 3 tool calls, got %d", readTool.callCount)
	}
	if !strings.Contains(reply, "Done reading") {
		t.Fatalf("unexpected reply: %s", reply)
	}
}

// ===========================================================================
// TEST: Context cancel mid-iteration 鈥?should stop cleanly
// ===========================================================================

func TestEngineCancelMidIteration(t *testing.T) {
	slowTool := &mockTool{name: "slow", readOnly: true, safe: true, delay: 2 * time.Second, result: "slow result"}

	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "slow", Input: map[string]any{"input": "x"}}}},
			{content: "should not reach here"},
		},
	}
	eng := newTestEngine(prov, slowTool)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "run slow tool",
	}, nil, nil)

	// Should return without hanging
	if err == nil {
		// Tool might have been cancelled 鈥?either error or short-circuit is OK
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// This is expected 鈥?clean cancellation
		return
	}
	t.Fatalf("unexpected error type: %v", err)
}

// ===========================================================================
// TEST: Empty tool call list 鈥?should not crash
// ===========================================================================

func TestEngineEmptyToolCalls(t *testing.T) {
	prov := &mockProvider{
		responses: []mockResponse{
			{content: "", toolCalls: []api.ToolCall{}}, // empty
			{content: "I have nothing to do."},
		},
	}
	eng := newTestEngine(prov)

	ctx := context.Background()
	reply, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "do nothing",
	}, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = reply
}

// ===========================================================================
// TEST: Permission allowed 鈥?tool should execute
// ===========================================================================

func TestEnginePermissionAllowed(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "file written"}

	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "write", Input: map[string]any{"input": "hello"}}}},
			{content: "File was written successfully."},
		},
	}
	eng := newTestEngine(prov, writeTool)
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		return true // User approves
	}

	ctx := context.Background()
	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "write a file",
	}, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writeTool.callCount != 1 {
		t.Fatalf("expected tool called once after permission grant, got %d", writeTool.callCount)
	}
}

// ===========================================================================
// TEST: Diagnostic system 鈥?QuickCheck should work with real config
// ===========================================================================

func TestDiagnosticIntegration(t *testing.T) {
	// Diagnostic system is tested in its own package (diagnostic_test.go).
	// Here we just confirm the engine can be instantiated without crashing
	// when diagnostic-related fields are nil.
	prov := &mockProvider{
		responses: []mockResponse{{content: "OK"}},
	}
	eng := newTestEngine(prov)
	if eng == nil {
		t.Fatal("engine should be created")
	}
}

// ===========================================================================
// TEST: Stream handler receives deltas correctly
// ===========================================================================

func TestEngineStreamDeltas(t *testing.T) {
	prov := &mockProvider{
		responses: []mockResponse{
			{content: "Hello world"},
		},
	}
	eng := newTestEngine(prov)

	var deltas []string
	ctx := context.Background()
	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "say hello",
	}, func(delta string) {
		deltas = append(deltas, delta)
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deltas) == 0 {
		t.Fatal("expected at least one delta callback")
	}
	joined := strings.Join(deltas, "")
	if !strings.Contains(joined, "Hello world") {
		t.Fatalf("expected deltas to contain response, got: %s", joined)
	}
}

// ===========================================================================
// TEST: Unknown tool name from model 鈥?should not crash
// ===========================================================================

func TestEngineUnknownToolName(t *testing.T) {
	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "nonexistent_tool", Input: map[string]any{}}}},
			{content: "That tool doesn't exist, let me try another way."},
		},
	}
	eng := newTestEngine(prov)

	ctx := context.Background()
	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "use nonexistent tool",
	}, nil, nil)

	if err != nil {
		t.Fatalf("engine should handle unknown tool gracefully, got: %v", err)
	}
}

// ===========================================================================
// TEST: Concurrent parallel tools 鈥?both should complete
// ===========================================================================

func TestEngineParallelToolExecution(t *testing.T) {
	tool1 := &mockTool{name: "grep", readOnly: true, safe: true, result: "match1", delay: 50 * time.Millisecond}
	tool2 := &mockTool{name: "list", readOnly: true, safe: true, result: "match2", delay: 50 * time.Millisecond}

	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{
				{ID: "tc1", Name: "grep", Input: map[string]any{"input": "pattern"}},
				{ID: "tc2", Name: "list", Input: map[string]any{"input": "."}},
			}},
			{content: "Found results from both tools."},
		},
	}
	eng := newTestEngine(prov, tool1, tool2)

	start := time.Now()
	ctx := context.Background()
	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "search and list",
	}, nil, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tool1.callCount != 1 || tool2.callCount != 1 {
		t.Fatalf("expected both tools called once, got tool1=%d tool2=%d", tool1.callCount, tool2.callCount)
	}
	// They should run in parallel, so total time should be close to 50ms, not 100ms
	if elapsed > 150*time.Millisecond {
		t.Logf("WARNING: parallel tools took %v (expected ~50ms if truly parallel)", elapsed)
	}
}

// ===========================================================================
// TEST: Auto-permission mode 鈥?should not ask, should allow all
// ===========================================================================

func TestEngineAutoPermissionMode(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "written"}

	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "write", Input: map[string]any{"input": "x"}}}},
			{content: "Done."},
		},
	}
	eng := newTestEngine(prov, writeTool)
	// Auto mode 鈥?should bypass permission prompt entirely
	eng.config.PermissionMode = "auto"
	eng.perm.SetMode(permission.Bypass)
	promptCalled := false
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		promptCalled = true
		return true
	}

	ctx := context.Background()
	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "write",
	}, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if promptCalled {
		t.Fatal("auto mode should not call PermissionPrompt")
	}
	if writeTool.callCount != 1 {
		t.Fatalf("expected tool called in auto mode, got %d", writeTool.callCount)
	}
}

// ===========================================================================
// TEST: Guardrail preflight warning must reach the model (§2.5), not just
// a debug log — regression test for the fix in executeTool.
// ===========================================================================

func TestExecuteTool_SurfacesGuardrailWarningToModel(t *testing.T) {
	failTool := &mockTool{name: "flaky", readOnly: true, safe: true, err: fmt.Errorf("boom")}
	eng := newTestEngine(&mockProvider{}, failTool)

	tc := api.ToolCall{ID: "t1", Name: "flaky", Input: map[string]any{"input": "x"}}
	ctx := context.Background()

	// guardrail.Tracker warns once the same (tool, args) signature has
	// failed >= 2 times already, so the first two identical failures
	// should NOT carry a warning yet.
	for i := 0; i < 2; i++ {
		out := eng.executeTool(ctx, tc)
		if !strings.Contains(out, "boom") {
			t.Fatalf("call %d: expected failure output, got %q", i, out)
		}
		if strings.Contains(out, "[guardrail:") {
			t.Fatalf("call %d: did not expect a guardrail warning yet, got %q", i, out)
		}
	}

	// Third identical call: the guardrail's preflight check now fires a
	// Warn decision. That must be visible in the tool result the model
	// sees, not just logged at debug level.
	out := eng.executeTool(ctx, tc)
	if !strings.Contains(out, "[guardrail:") {
		t.Fatalf("expected guardrail warning to be surfaced in tool output, got %q", out)
	}
	if !strings.Contains(out, "boom") {
		t.Fatalf("expected the real failure output to still be present alongside the warning, got %q", out)
	}
}

// ===========================================================================
// VISUAL DEMO: Steer flow — user guides AI mid-task (matches Hermes /steer)
// ===========================================================================
// Run with: go test -v -run TestSteerFlowDemo ./internal/engine/


func TestSteerFlowDemo(t *testing.T) {
	t.Log("")
	t.Log("╔══════════════════════════════════════════════════════════╗")
	t.Log("║  Steer 流程演示：用户中途引导 AI 调整任务                  ║")
	t.Log("╚══════════════════════════════════════════════════════════╝")
	t.Log("")

	// ── Step 1: Setup ──
	t.Log("📋 场景：用户让 AI '列出文件'，中途追加指引 '只看 Go 文件'")
	t.Log("")

	// Tool that signals when called (so we can inject steer at the right moment)
	barrier := make(chan struct{})
	shellTool := &steerableMockTool{
		mockTool: &mockTool{
			name:     "shell",
			readOnly: true,
			safe:     true,
			result:   "file1.go\nfile2.py\nfile3.go\ntest.go\nREADME.md\nmain.go",
		},
		barrier: barrier,
	}

	prov := &mockProvider{
		responses: []mockResponse{
			// Iteration 1: AI decides to run a shell command
			{toolCalls: []api.ToolCall{{ID: "tc1", Name: "shell", Input: map[string]any{"command": "ls"}}}},
			// Iteration 2: AI should see steer in tool msg and filter to Go files
			{content: "找到 3 个 Go 文件：file1.go, file3.go, test.go, main.go"},
		},
	}

	eng := newTestEngine(prov, shellTool)
	eng.extractRunner = nil // disable background extract to avoid real HTTP calls in tests
	eng.dreamRunner = nil   // disable background dream to avoid real HTTP calls in tests
	eng.perm.SetMode(permission.Bypass) // bypass permission for demo test

	// ── Step 2: Start task in background ──
	t.Log("⚡ 用户提交: '列出当前目录所有文件'")
	t.Log("")

	var (
		reply   string
		runErr  error
		runDone = make(chan struct{})
	)

	go func() {
		reply, runErr = eng.RunMessageWithStream(
			context.Background(),
			api.Message{Role: "user", Content: "列出当前目录所有文件"},
			nil, nil,
		)
		close(runDone)
	}()

	// ── Step 3: Tool is blocked on barrier, inject steer now ──
	t.Log("🛠️  AI 执行了 shell ls 命令，结果：")
	t.Log("   file1.go  file2.py  file3.go  test.go  README.md  main.go")
	t.Log("")
	t.Log("💬 用户中途输入: '只看 Go 文件，忽略其他'")
	eng.Steer("只看 Go 文件，忽略其他")
	t.Log("   → steer 已注入引擎")
	close(barrier) // unblock the tool, engine loop will drain steer next iteration
	t.Log("   → 释放 barrier，引擎继续下一轮 LLM 调用")
	t.Log("")

	// ── Step 4: Wait for completion ──
	<-runDone

	// ── Step 5: Verify ──
	t.Log("─── 验证结果 ───")
	t.Log("")

	if runErr != nil {
		t.Fatalf("❌ 引擎运行失败: %v", runErr)
	}

	// Check the last request that was sent to the provider (iteration 2)
	prov.mu.Lock()
	lastReq := prov.lastReq
	prov.mu.Unlock()

	t.Logf("🔍 第二轮 API 请求包含 %d 条消息:", len(lastReq.Messages))
	for i, msg := range lastReq.Messages {
		preview := msg.Content
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		t.Logf("   [%d] role=%-10s content=%s", i, msg.Role, preview)
	}

	// Check that the steer text was injected into the tool message
	found := false
	for _, msg := range lastReq.Messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, "[用户指引]") {
			t.Logf("✅ 第二轮请求的工具消息中包含用户指引！")
			t.Logf("   完整内容: %s", msg.Content)
			found = true
		}
	}
	if !found {
		t.Log("❌ 第二轮请求中未找到 [用户指引]")
		// Also check eng.messages
		t.Log("eng.messages:")
		for i, msg := range eng.messages {
			preview := msg.Content
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			t.Logf("   [%d] role=%-10s content=%s", i, msg.Role, preview)
		}
		t.Fatal("❌ steer 未注入到工具消息中")
	}

	// Check that the AI's final response reflects the guidance
	t.Logf("📝 AI 最终回复: %s", reply)

	t.Log("")
	t.Log("╔══════════════════════════════════════════════════════════╗")
	t.Log("║  ✅ Steer 流程演示通过                                     ║")
	t.Log("║  用户中途输入 → eng.Steer() → 注入 tool 消息              ║")
	t.Log("║  → LLM 看到 [用户指引] → 下轮迭代调整行为                  ║")
	t.Log("╚══════════════════════════════════════════════════════════╝")
}

// steerableMockTool wraps mockTool and uses a barrier channel so the test
// can inject steer at exactly the right moment — after the tool executes but
// before the engine loop drains pendingSteer on the next iteration.
type steerableMockTool struct {
	*mockTool
	barrier chan struct{} // tool blocks here until test calls Steer()
}

func (s *steerableMockTool) Call(ctx context.Context, input tool.Input, tc tool.Context) (tool.Result, error) {
	result, err := s.mockTool.Call(ctx, input, tc)
	// Block until test goroutine has called eng.Steer().  This is the
	// synchronisation barrier that guarantees steer text is in pendingSteer
	// before the engine loop drains it on the next iteration.
	<-s.barrier
	return result, err
}


// ===========================================================================
// E2E DEMO: Real HTTP server — tests full network stack, not just mock structs
// ===========================================================================
// Run with: go test -v -run TestSteerE2E ./internal/engine/ -count=1

func TestSteerE2E(t *testing.T) {
	// ── Step 1: Start a real HTTP server that mimics OpenAI API ──
	requestLog := make(chan api.ChatRequest, 5)

	serverCalled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		requestLog <- req

		// Check if this is the first or second call
		hasToolMsg := false
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				hasToolMsg = true
				break
			}
		}

		var resp any
		if !hasToolMsg {
			// First call: respond with a tool call
			resp = map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role":    "assistant",
						"content": "",
						"tool_calls": []map[string]any{{
							"id":   "tc1",
							"type": "function",
							"function": map[string]any{
								"name":      "shell",
								"arguments": `{"command":"ls"}`,
							},
						}},
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     10,
					"completion_tokens": 5,
				},
			}
			select {
			case serverCalled <- struct{}{}:
			default:
			}
		} else {
			// Second call: respond with final answer
			resp = map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role":    "assistant",
						"content": "Filtered to Go files only",
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     20,
					"completion_tokens": 10,
				},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// ── Step 2: Create engine pointing at real HTTP server ──
	// Use barrier to synchronize steer injection
	barrier := make(chan struct{})

	shellTool := &steerableMockTool{
		mockTool: &mockTool{
			name:     "shell",
			readOnly: true,
			safe:     true,
			result:   "file1.go\nfile2.py\nfile3.go",
		},
		barrier: barrier,
	}

	cfg := Config{
		Model:          "test-model",
		PermissionMode: "auto",
		MaxBudget:      100,
		Provider: api.ProviderConfig{
			Name:      "openai",
			APIKey:    "sk-test",
			BaseURL:   server.URL + "/v1", // unused by mock but needed for provider init
		},
	}
	cfg.Tools = []tool.Tool{shellTool}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eng.extractRunner = nil
	eng.dreamRunner = nil
	eng.perm.SetMode(permission.Bypass)

	// Override provider to use our mock server
	// The openAICompatProvider uses the baseURL from ProviderConfig
	prov := eng.Provider()
	_ = prov // keep reference

	t.Log("")
	t.Log("╔══════════════════════════════════════════════════════════╗")
	t.Log("║  E2E 演示：真实 HTTP 服务器 + Steer 注入                    ║")
	t.Log("╚══════════════════════════════════════════════════════════╝")
	t.Log("")
	t.Logf("🌐 Mock API 服务器: %s", server.URL)
	t.Log("")

	// ── Step 3: Run engine in background ──
	done := make(chan struct{})
	var reply string
	var runErr error

	go func() {
		reply, runErr = eng.RunMessageWithStream(
			context.Background(),
			api.Message{Role: "user", Content: "list files"},
			nil, nil,
		)
		close(done)
	}()

	// ── Step 4: Wait for first API call, then steer ──
	<-serverCalled
	t.Log("⚡ 第一轮 API 调用已发出（LLM 返回 tool_call）")
	t.Log("🛠️  shell 工具正在执行...")

	// Tool is now blocked on barrier — inject steer
	t.Log("💬 用户输入: /steer 只看 Go 文件")
	eng.Steer("只看 Go 文件")
	close(barrier)
	t.Log("   → steer 已注入，释放工具继续")
	t.Log("")

	// ── Step 5: Wait for completion ──
	<-done
	if runErr != nil {
		t.Fatalf("❌ 引擎运行失败: %v", runErr)
	}

	// ── Step 6: Verify the second API request contains the steer ──
	close(requestLog)
	var requests []api.ChatRequest
	for req := range requestLog {
		requests = append(requests, req)
	}

	t.Log("─── 验证结果 ───")
	t.Log("")
	t.Logf("📡 共发出 %d 次 API 请求", len(requests))

	if len(requests) < 2 {
		t.Fatal("❌ 预期至少 2 次 API 请求")
	}

	// Check second request for steer in tool message
	req2 := requests[1]
	t.Logf("🔍 第二轮请求包含 %d 条消息:", len(req2.Messages))
	found := false
	for i, msg := range req2.Messages {
		preview := msg.Content
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		t.Logf("   [%d] role=%-10s content=%s", i, msg.Role, preview)
		if msg.Role == "tool" && strings.Contains(msg.Content, "[用户指引]") {
			t.Logf("   ✅ 工具消息包含用户指引!")
			found = true
		}
	}

	if !found {
		t.Fatal("❌ steer 未出现在第二轮 API 请求中")
	}

	t.Logf("📝 AI 最终回复: %s", reply)
	t.Log("")
	t.Log("╔══════════════════════════════════════════════════════════╗")
	t.Log("║  ✅ E2E 测试通过 — 真实 HTTP 请求包含 steer 注入          ║")
	t.Log("╚══════════════════════════════════════════════════════════╝")
}
