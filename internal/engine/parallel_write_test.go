package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// overlapTool reports how many of its own calls were executing at the same
// time, both overall and per file path. It is deliberately separate from
// mockTool: this test's whole point is concurrent execution, so the probe's
// own bookkeeping has to be race-free, and mockTool's plain callCount++ is not.
//
// It declares IsConcurrencySafe: false and is named "write", which is exactly
// the case the dispatch loop special-cases in engine.go.
type overlapTool struct {
	name  string
	delay time.Duration

	mu sync.Mutex
	// inFlight and maxInFlight count calls currently executing, across all paths.
	inFlight    int
	maxInFlight int
	// perPath / maxPerPath do the same, keyed by the filePath input.
	perPath    map[string]int
	maxPerPath map[string]int
}

func (t *overlapTool) Def() tool.Def {
	return tool.Def{
		Name:              t.name,
		Description:       "probe that records concurrent invocations",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"filePath":{"type":"string"}}}`),
		IsReadOnly:        false,
		IsConcurrencySafe: false,
		UserFacingName:    t.name,
	}
}

func (t *overlapTool) Validate(input tool.Input) string { return "" }

func (t *overlapTool) CheckPermissions(input tool.Input, tctx tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Allow}
}

func (t *overlapTool) Call(ctx context.Context, input tool.Input, tctx tool.Context) (tool.Result, error) {
	fp, _ := input["filePath"].(string)

	t.mu.Lock()
	if t.perPath == nil {
		t.perPath = map[string]int{}
		t.maxPerPath = map[string]int{}
	}
	t.inFlight++
	t.perPath[fp]++
	if t.inFlight > t.maxInFlight {
		t.maxInFlight = t.inFlight
	}
	if t.perPath[fp] > t.maxPerPath[fp] {
		t.maxPerPath[fp] = t.perPath[fp]
	}
	t.mu.Unlock()

	select {
	case <-time.After(t.delay):
	case <-ctx.Done():
	}

	t.mu.Lock()
	t.inFlight--
	t.perPath[fp]--
	t.mu.Unlock()

	return tool.Result{Data: "wrote " + fp}, nil
}

func (t *overlapTool) peakInFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxInFlight
}

func (t *overlapTool) peakForPath(fp string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxPerPath[fp]
}

// Two write calls to the same file in a single response must not run at the
// same time — that is what serialFilePaths exists to prevent. The dispatch loop
// claims paths as it walks the batch; a path that is already claimed has to be
// deferred until the parallel batch has drained, not merely moved to the loop's
// inline branch, because the inline branch executes immediately and therefore
// races the goroutine it was supposed to wait for.
func TestEngineSerializesWritesToTheSameFile(t *testing.T) {
	write := &overlapTool{name: "write", delay: 80 * time.Millisecond}
	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{
				{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}},
				{ID: "tc2", Name: "write", Input: map[string]any{"filePath": "a.go"}},
			}},
			{content: "wrote a.go twice"},
		},
	}
	eng := newTestEngine(prov, write)

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{
		Role: "user", Content: "write a.go twice",
	}, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := write.peakForPath("a.go"); got != 1 {
		t.Errorf("two writes to a.go were in flight at once (%d); same-file writes must be serialized", got)
	}
	if got := write.peakInFlight(); got != 1 {
		t.Errorf("peak concurrent tool calls was %d, want 1: nothing else was in the batch, so the second write ran alongside the first", got)
	}
}

// The counterpart guard: same-file writes are serialized by deferring the
// duplicate, not by serializing everything. Writes to distinct files are the
// case the optimization exists for and must still overlap.
func TestEngineParallelizesWritesToDifferentFiles(t *testing.T) {
	write := &overlapTool{name: "write", delay: 80 * time.Millisecond}
	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{
				{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}},
				{ID: "tc2", Name: "write", Input: map[string]any{"filePath": "b.go"}},
			}},
			{content: "wrote both"},
		},
	}
	eng := newTestEngine(prov, write)

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{
		Role: "user", Content: "write a.go and b.go",
	}, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := write.peakInFlight(); got != 2 {
		t.Errorf("peak concurrent tool calls was %d, want 2: writes to different files should still run in parallel", got)
	}
}
