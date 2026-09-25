package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// spanTool records when each of its calls started and ended.
type spanTool struct {
	name  string
	safe  bool
	delay time.Duration

	mu    sync.Mutex
	spans map[string][2]time.Time // tool-call key -> [start, end]
}

func (t *spanTool) Def() tool.Def {
	return tool.Def{
		Name:              t.name,
		Description:       "probe that records call spans",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"filePath":{"type":"string"}}}`),
		IsReadOnly:        t.safe,
		IsConcurrencySafe: t.safe,
		UserFacingName:    t.name,
	}
}

func (t *spanTool) Validate(tool.Input) string { return "" }

func (t *spanTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Allow}
}

func (t *spanTool) Call(ctx context.Context, input tool.Input, _ tool.Context) (tool.Result, error) {
	key, _ := input["filePath"].(string)
	start := time.Now()
	select {
	case <-time.After(t.delay):
	case <-ctx.Done():
	}
	end := time.Now()
	t.mu.Lock()
	if t.spans == nil {
		t.spans = map[string][2]time.Time{}
	}
	t.spans[key] = [2]time.Time{start, end}
	t.mu.Unlock()
	return tool.Result{Data: "ok " + key}, nil
}

func (t *spanTool) span(key string) [2]time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spans[key]
}

func runBatch(t *testing.T, calls []api.ToolCall, tools ...tool.Tool) {
	t.Helper()
	prov := &mockProvider{responses: []mockResponse{{toolCalls: calls}, {content: "done"}}}
	eng := newTestEngine(prov, tools...)
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "go"}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// A serial tool listed after a concurrency-safe one used to run inline while
// the safe one was still in flight: [read(slow), bash] ran bash during the
// read. Safe calls now drain before any serial call starts.
func TestSerialToolWaitsForParallelSafeGroup(t *testing.T) {
	read := &spanTool{name: "read", safe: true, delay: 200 * time.Millisecond}
	bash := &spanTool{name: "slowbash"}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "read", Input: map[string]any{"filePath": "r"}},
		{ID: "2", Name: "slowbash", Input: map[string]any{"filePath": "b"}},
	}, read, bash)

	r, b := read.span("r"), bash.span("b")
	if r[1].IsZero() || b[0].IsZero() {
		t.Fatalf("a call did not run: read=%v bash=%v", r, b)
	}
	if b[0].Before(r[1]) {
		t.Fatalf("serial call started %v before the safe read finished", r[1].Sub(b[0]))
	}
}

// A write that follows a serial call depends on its side effects:
// [bash "mkdir d", write d/f] must not write before the directory exists.
// Writes to distinct files are parallelizable, but only among the calls
// after the last serial one.
func TestWriteAfterSerialCallWaitsForIt(t *testing.T) {
	bash := &spanTool{name: "slowbash", delay: 200 * time.Millisecond}
	write := &spanTool{name: "write"}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "slowbash", Input: map[string]any{"filePath": "b"}},
		{ID: "2", Name: "write", Input: map[string]any{"filePath": "d/f"}},
	}, bash, write)

	b, w := bash.span("b"), write.span("d/f")
	if b[1].IsZero() || w[0].IsZero() {
		t.Fatalf("a call did not run: bash=%v write=%v", b, w)
	}
	if w[0].Before(b[1]) {
		t.Fatalf("write started %v before the serial call before it finished", b[1].Sub(w[0]))
	}
}

// Edits to distinct files still run in parallel.
func TestEditsToDistinctFilesStillOverlap(t *testing.T) {
	const d = 150 * time.Millisecond
	edit := &spanTool{name: "edit", delay: d}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "edit", Input: map[string]any{"filePath": "a.go"}},
		{ID: "2", Name: "edit", Input: map[string]any{"filePath": "b.go"}},
	}, edit)
	a, b := edit.span("a.go"), edit.span("b.go")
	if a[1].IsZero() || b[1].IsZero() {
		t.Fatal("an edit did not run")
	}
	// The two calls' own intervals must overlap; comparing the total span
	// against a wall-clock bound was flaky on a loaded machine.
	if !a[0].Before(b[1]) || !b[0].Before(a[1]) {
		t.Fatalf("edits to distinct files ran serially: a=[%v,%v] b=[%v,%v]", a[0], a[1], b[0], b[1])
	}
}

// orderTool records the IDs of its calls in the order they ran, across every
// tool sharing the same log.
type orderTool struct {
	name string
	safe bool
	log  *orderLog
}

type orderLog struct {
	mu  sync.Mutex
	ids []string
}

func (t *orderTool) Def() tool.Def {
	return tool.Def{
		Name:              t.name,
		Description:       "probe that records call order",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"filePath":{"type":"string"},"id":{"type":"string"}}}`),
		IsConcurrencySafe: t.safe,
		UserFacingName:    t.name,
	}
}

func (t *orderTool) Validate(tool.Input) string { return "" }

func (t *orderTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Allow}
}

func (t *orderTool) Call(_ context.Context, input tool.Input, _ tool.Context) (tool.Result, error) {
	id, _ := input["id"].(string)
	t.log.mu.Lock()
	t.log.ids = append(t.log.ids, id)
	t.log.mu.Unlock()
	return tool.Result{Data: "ok"}, nil
}

// A same-file duplicate write is not held until the end of the batch: it
// runs before the next serial call, which may depend on it.
// [write x, bash, write x, bash2] runs the second write before bash2.
func TestDeferredWriteRunsBeforeLaterSerialCall(t *testing.T) {
	log := &orderLog{}
	write := &orderTool{name: "write", log: log}
	bash := &orderTool{name: "slowbash", log: log}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "write", Input: map[string]any{"filePath": "x", "id": "write1"}},
		{ID: "2", Name: "slowbash", Input: map[string]any{"id": "bash"}},
		{ID: "3", Name: "write", Input: map[string]any{"filePath": "x", "id": "write2"}},
		{ID: "4", Name: "slowbash", Input: map[string]any{"id": "bash2"}},
	}, write, bash)
	got := strings.Join(log.ids, ",")
	if want := "write1,bash,write2,bash2"; got != want {
		t.Fatalf("call order = %s, want %s", got, want)
	}
}

// A same-file duplicate before a serial call runs before that call.
func TestDeferredWriteRunsAtBarrier(t *testing.T) {
	log := &orderLog{}
	write := &orderTool{name: "write", log: log}
	bash := &orderTool{name: "slowbash", log: log}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "write", Input: map[string]any{"filePath": "x", "id": "write1"}},
		{ID: "2", Name: "write", Input: map[string]any{"filePath": "x", "id": "write2"}},
		{ID: "3", Name: "slowbash", Input: map[string]any{"id": "bash"}},
	}, write, bash)
	got := strings.Join(log.ids, ",")
	if want := "write1,write2,bash"; got != want {
		t.Fatalf("call order = %s, want %s", got, want)
	}
}
