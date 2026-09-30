package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// /new and /resume reset fileHistory to nil, and the next successful write or
// edit assigned into it: "assignment to entry in nil map". The file was
// already written, but the call came back to the model as a failure.
func TestFileTrackingSurvivesNewSession(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.NewSession(context.Background())
	path := filepath.Join(t.TempDir(), "a.go")
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("trackFileChanges panicked after /new: %v", r)
			}
		}()
		eng.trackFileChanges(api.ToolCall{Name: "write", Input: map[string]any{"filePath": path}})
	}()
	if !eng.fileHistory[path] {
		t.Fatalf("write after /new was not recorded: %v", eng.fileHistory)
	}
}

// write and edit accept file_path, path, filepath and file as well as
// filePath; tracking read only "filePath", so a call using an alias changed
// the file without the turn knowing it had.
func TestFileTrackingHonorsPathAliases(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	for _, key := range []string{"file_path", "path", "filepath", "file"} {
		eng := newTestEngine(&mockProvider{})
		path := filepath.Join(dir, key+".go")
		eng.trackFileChanges(api.ToolCall{Name: "edit", Input: map[string]any{key: path}})
		if !eng.turnChangedFiles[filepath.Clean(path)] {
			t.Errorf("edit with %q was not recorded as a changed file: %v", key, eng.turnChangedFiles)
		}
	}
}

// A read listed after a write or edit in the same batch ran concurrently
// with it and could see the file before, or halfway through, the change.
// It now waits for the writes started before it.
func TestReadAfterEditInBatchWaitsForIt(t *testing.T) {
	edit := &spanTool{name: "edit", delay: 200 * time.Millisecond}
	read := &spanTool{name: "read", safe: true}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "edit", Input: map[string]any{"filePath": "p.go"}},
		{ID: "2", Name: "read", Input: map[string]any{"filePath": "r"}},
	}, edit, read)
	e, r := edit.span("p.go"), read.span("r")
	if e[1].IsZero() || r[0].IsZero() {
		t.Fatalf("a call did not run: edit=%v read=%v", e, r)
	}
	if r[0].Before(e[1]) {
		t.Fatalf("read started %v before the earlier edit finished", e[1].Sub(r[0]))
	}
}

// The reverse order: a write listed after a read must not change the file
// while the read is still looking at it.
func TestWriteAfterReadInBatchWaitsForIt(t *testing.T) {
	read := &spanTool{name: "read", safe: true, delay: 200 * time.Millisecond}
	write := &spanTool{name: "write"}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "read", Input: map[string]any{"filePath": "r"}},
		{ID: "2", Name: "write", Input: map[string]any{"filePath": "p.go"}},
	}, read, write)
	r, w := read.span("r"), write.span("p.go")
	if r[1].IsZero() || w[0].IsZero() {
		t.Fatalf("a call did not run: read=%v write=%v", r, w)
	}
	if w[0].Before(r[1]) {
		t.Fatalf("write started %v before the earlier read finished", r[1].Sub(w[0]))
	}
}

// Reads after the writes still run in parallel with each other.
func TestReadsAfterWriteStillOverlap(t *testing.T) {
	const d = 150 * time.Millisecond
	write := &spanTool{name: "write"}
	read := &spanTool{name: "read", safe: true, delay: d}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "write", Input: map[string]any{"filePath": "p.go"}},
		{ID: "2", Name: "read", Input: map[string]any{"filePath": "a"}},
		{ID: "3", Name: "read", Input: map[string]any{"filePath": "b"}},
	}, write, read)
	a, b := read.span("a"), read.span("b")
	if a[1].IsZero() || b[1].IsZero() {
		t.Fatal("a read did not run")
	}
	if !a[0].Before(b[1]) || !b[0].Before(a[1]) {
		t.Fatalf("reads after a write ran serially: a=[%v,%v] b=[%v,%v]", a[0], a[1], b[0], b[1])
	}
}

// A same-file duplicate write held back in the group runs before a later
// read, which has to see the final content.
func TestDeferredWriteRunsBeforeLaterRead(t *testing.T) {
	log := &orderLog{}
	write := &orderTool{name: "write", log: log}
	read := &orderTool{name: "read", safe: true, log: log}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "write", Input: map[string]any{"filePath": "x", "id": "write1"}},
		{ID: "2", Name: "write", Input: map[string]any{"filePath": "x", "id": "write2"}},
		{ID: "3", Name: "read", Input: map[string]any{"filePath": "x", "id": "read"}},
	}, write, read)
	if got, want := strings.Join(log.ids, ","), "write1,write2,read"; got != want {
		t.Fatalf("call order = %s, want %s", got, want)
	}
}

// Layer-1 trimming alone returns Compressed with the original history, whose
// first message is the user's real request; the task list was appended to it
// on every such compaction.
func TestTodoListNotAppendedToUntouchedHistory(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos(sampleTodos())
	eng.messages = []api.Message{{Role: "user", Content: "fix the parser"}, {Role: "assistant", Content: "ok"}}
	eng.todoAfterCompaction(false)
	if eng.messages[0].Content != "fix the parser" {
		t.Fatalf("the user's request was changed by a compaction that kept it: %q", eng.messages[0].Content)
	}
}

// A summary that already carries a task list block gets the current one in
// its place, not a second one.
func TestTodoListAfterCompactionNotDuplicated(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos(sampleTodos())
	eng.messages = []api.Message{{Role: "user", Content: "<compress>summary</compress>"}, {Role: "assistant", Content: "ok"}}
	eng.todoAfterCompaction(true)
	eng.todoAfterCompaction(true)
	if n := strings.Count(eng.messages[0].Content, "<todo_list>"); n != 1 {
		t.Fatalf("%d task list blocks on the summary, want 1:\n%s", n, eng.messages[0].Content)
	}
	if !strings.Contains(eng.messages[0].Content, "fix the tokenizer") {
		t.Fatalf("summary lacks the task list: %q", eng.messages[0].Content)
	}
}

// The read tool's continuation marker is documented to stay the last line;
// a guardrail warning was appended after it.
func TestGuardrailWarningKeepsReadMarkerLast(t *testing.T) {
	data := "File: a.go (30 lines total)\n\n1: package a\n2: \n... [showing lines 1-2 of 30]\n[next: offset=3]"
	mt := &mockTool{name: "read", readOnly: true, result: data}
	eng := newTestEngine(&mockProvider{}, mt)
	in := map[string]any{"filePath": "a.go"}
	for i := 0; i < 3; i++ {
		eng.guardrails.AfterCall("read", in, data, false)
	}
	out, _ := eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "read", Input: in})
	if !strings.Contains(out, "[guardrail:") {
		t.Fatalf("fixture did not trigger a guardrail warning: %q", out)
	}
	if !strings.HasSuffix(out, "\n[next: offset=3]") {
		t.Fatalf("continuation marker is not the last line:\n%s", out)
	}
	if strings.Count(out, "[next: offset=") != 1 {
		t.Fatalf("marker duplicated:\n%s", out)
	}
}

// The stall notice went out as a line starting with "\r\x1b[K": cursor
// control a front end with a live region must never receive.
func TestStallNoticeHasNoCursorControl(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	eng.reportStall("bash", 40*time.Second, false)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), lines)
	}
	if strings.ContainsAny(lines[0], "\r") || strings.Contains(lines[0], "\x1b[K") {
		t.Fatalf("stall line carries cursor control: %q", lines[0])
	}
	if !strings.Contains(lines[0], "bash") {
		t.Fatalf("stall line lost the stage: %q", lines[0])
	}
}
