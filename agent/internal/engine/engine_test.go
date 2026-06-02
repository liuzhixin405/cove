package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/permission"
	"github.com/agentgo/internal/tool"
)

// ===========================================================================
// Mock Provider — simulates API responses for integration testing
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
// Mock Tool — controllable tool for testing permission/execution paths
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
	eng.provider = provider
	return eng
}

// ===========================================================================
// TEST: Basic message flow — send message, get response
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
	})

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
// TEST: Context cancellation — Ctrl+C during API call
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
	}, nil)

	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected context error, got: %v", err)
	}
}

// ===========================================================================
// TEST: Tool execution — model requests tool, tool runs, result returned
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
	}, nil)

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
// TEST: Permission denied — tool requires permission, user denies
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
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Tool should NOT have been called
	if writeTool.callCount != 0 {
		t.Fatalf("expected tool NOT called (permission denied), got %d calls", writeTool.callCount)
	}
}

// ===========================================================================
// TEST: Permission prompt not set — should not hang, should deny gracefully
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
	eng.PermissionPrompt = nil // NOT SET — this should not hang

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := eng.RunMessageWithStream(ctx, api.Message{
		Role: "user", Content: "write file",
	}, nil)

	if err != nil {
		// Should not timeout — if it does, there's a hang
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
// TEST: Tool panic recovery — goroutine panic should not crash process
// ===========================================================================

func TestEngineToolPanicRecovery(t *testing.T) {
	panicTool := &mockTool{name: "read", readOnly: true, safe: true, panicMsg: "unexpected nil pointer"}
	normalTool := &mockTool{name: "list", readOnly: true, safe: true, result: "a.txt\nb.txt"}

	prov := &mockProvider{
		responses: []mockResponse{
			// Model requests two tools in parallel — one panics
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
	}, nil)

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
// TEST: API error — should return error, not hang
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
	}, nil)

	if err == nil {
		t.Fatal("expected error from API failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("HANG DETECTED: engine blocked on API error instead of returning")
	}
}

// ===========================================================================
// TEST: Multiple iterations — model calls tools across multiple turns
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
	}, nil)

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
// TEST: Context cancel mid-iteration — should stop cleanly
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
	}, nil)

	// Should return without hanging
	if err == nil {
		// Tool might have been cancelled — either error or short-circuit is OK
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// This is expected — clean cancellation
		return
	}
	t.Fatalf("unexpected error type: %v", err)
}

// ===========================================================================
// TEST: Empty tool call list — should not crash
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
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = reply
}

// ===========================================================================
// TEST: Permission allowed — tool should execute
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
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writeTool.callCount != 1 {
		t.Fatalf("expected tool called once after permission grant, got %d", writeTool.callCount)
	}
}

// ===========================================================================
// TEST: Diagnostic system — QuickCheck should work with real config
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
	})

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
// TEST: Unknown tool name from model — should not crash
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
	}, nil)

	if err != nil {
		t.Fatalf("engine should handle unknown tool gracefully, got: %v", err)
	}
}

// ===========================================================================
// TEST: Concurrent parallel tools — both should complete
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
	}, nil)
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
// TEST: Auto-permission mode — should not ask, should allow all
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
	// Auto mode — should bypass permission prompt entirely
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
	}, nil)

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
