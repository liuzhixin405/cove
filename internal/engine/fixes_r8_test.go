package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/tool"
	"github.com/liuzhixin405/cove-agent/internal/uiout"
)

// ---------------------------------------------------------------------------
// 1. Alias-aware key arguments.
// ---------------------------------------------------------------------------

// Ten edits to ten different files via the file_path alias used to fingerprint
// as a bare "edit" each, so unrelated work tripped loop detection (Layer 1a).
func TestFingerprintHonorsPathAliases(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	seen := map[string]bool{}
	for _, key := range []string{"filePath", "file_path", "path", "filepath", "file"} {
		for i := 0; i < 3; i++ {
			fp := eng.fingerprintToolCalls([]api.ToolCall{{Name: "edit", Input: map[string]any{key: "dir/f" + string(rune('a'+i)) + ".go"}}})
			if fp == "edit" {
				t.Fatalf("%s alias fingerprinted as a bare %q", key, fp)
			}
			seen[fp] = true
		}
	}
	// One file, whatever the alias, is one fingerprint: three distinct files.
	if len(seen) != 3 {
		t.Fatalf("fingerprints = %v, want 3 distinct files", seen)
	}
	// grep still keys on its pattern, not on the directory it searched.
	fp := eng.fingerprintToolCalls([]api.ToolCall{{Name: "grep", Input: map[string]any{"pattern": "TODO", "path": "internal"}}})
	if fp != "grep:TODO" {
		t.Fatalf("grep fingerprint = %q", fp)
	}
}

// The summary, the review snapshot and the tool result's context hints read
// the same key arguments, through the same alias-aware helper.
func TestSummaryAndReviewNamePathsOfAliasedCalls(t *testing.T) {
	msgs := []api.Message{
		{Role: "user", Content: "fix it"},
		{Role: "assistant", Content: "editing", ToolCalls: []api.ToolCall{
			{ID: "1", Name: "edit", Input: map[string]any{"file_path": "pkg/a.go", "old": "x", "new": "y"}},
			{ID: "2", Name: "write", Input: map[string]any{"path": "pkg/b.go", "content": "z"}},
		}},
	}
	snap := buildReviewSnapshot(msgs)
	for _, want := range []string{"edit(pkg" + string(filepath.Separator) + "a.go)", "write(pkg" + string(filepath.Separator) + "b.go)"} {
		if !strings.Contains(snap, want) {
			t.Errorf("review snapshot lacks %q:\n%s", want, snap)
		}
	}

	var sent api.ChatRequest
	cc := &ChatCompressor{}
	_, err := cc.generateSummary(context.Background(), msgs, func(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
		sent = req
		return &api.ChatResponse{Content: "summary"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body := sent.Messages[0].Content
	for _, want := range []string{"edit(pkg" + string(filepath.Separator) + "a.go)", "write(pkg" + string(filepath.Separator) + "b.go)"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary input lacks %q:\n%s", want, body)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Tool names are canonical before the batch is classified.
// ---------------------------------------------------------------------------

// aliasedWriter is a "write" tool registered with a capitalised alias, the
// way the real edit/write/powershell tools are.
type aliasedWriter struct{ fileWriteTool }

func (t *aliasedWriter) Def() tool.Def {
	d := t.fileWriteTool.Def()
	d.Aliases = []string{"Write"}
	return d
}

// A call emitted under an alias ("Write", "Edit") was classified by its raw
// name: checkpointBefore saw neither write nor edit, took no snapshot, and
// executeTool (which does normalize) skipped its own because the batch was
// already marked checkpointed. The file changed with nothing for /undo.
func TestAliasedWriteIsCheckpointed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "w1", Name: "Write", Input: map[string]any{"file_path": "a.txt"}}}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, &aliasedWriter{fileWriteTool{dir: dir}})
	cp, err := checkpoint.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	eng.cpMgr = cp
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "write"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(data) != "after" {
		t.Fatalf("setup: the aliased call did not write: %q", data)
	}
	if _, err := eng.RestoreCheckpoint(""); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(data) != "before" {
		t.Fatalf("a.txt = %q after undo, want the content from before the aliased write", data)
	}
	// The result is recorded under the canonical name, so every later layer
	// (blocks, verify evidence, todo tracking) sees "write".
	for _, m := range eng.Messages() {
		if m.Role == "tool" && m.Name != "write" {
			t.Fatalf("tool result recorded under %q, want the canonical name", m.Name)
		}
	}
}

// A PowerShell alias's non-zero exit is flagged like powershell's.
func TestEmitToolResultFlagsAliasedShellExit(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	capture := uiout.NewCapture()
	eng.SetOutput(capture)
	eng.emitToolResult("c1", "PowerShell", map[string]any{"command": "go test"}, "Command: go test\n--- FAIL: TestX\n[exit code: 1]", false, time.Second)
	if got := capture.Blocks(); len(got) != 1 || !got[0].IsError {
		t.Fatalf("blocks = %+v, want one failed block", got)
	}
}

// ---------------------------------------------------------------------------
// 3. The todo finish nudge never stops a turn that has its answer.
// ---------------------------------------------------------------------------

// With -p --max-turns N, answering at call N with open todo items used to be
// sent back for the todo nudge, and the next call hit the cap: the report was
// replaced by a limit error.
func TestTodoFinishNudgeYieldsToTheIterationCap(t *testing.T) {
	isolatedHome(t)
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "t1", Name: "todowrite", Input: map[string]any{"todos": sampleTodos()}}}},
		{content: "The report."},
	}}
	eng := newTestEngine(prov, &mockTool{name: "todowrite", readOnly: true, safe: true, result: "ok"})
	eng.SetMaxIterations(2)
	eng.runtime.SetTodos(sampleTodos())
	reply, err := run(t, eng, "do it")
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if reply != "The report." {
		t.Fatalf("reply = %q", reply)
	}
	if providerCalls(prov) != 2 {
		t.Fatalf("%d model calls, want 2 (no nudge past the cap)", providerCalls(prov))
	}
}

// ---------------------------------------------------------------------------
// 4. The overload fallback moves the whole turn, not only t.model.
// ---------------------------------------------------------------------------

// The fallback set t.model alone, so currentModel() (tool output limits, the
// window the context is sized for) kept answering with the failed model.
func TestOverloadFallbackUpdatesCurrentModel(t *testing.T) {
	isolatedHome(t)
	prov := &mockProvider{responses: []mockResponse{
		{err: &api.RetryableError{Status: 503, Msg: "high demand"}},
		{content: "done on the fast model"},
	}}
	eng := newTestEngine(prov)
	eng.config.ModelFast = "fast-model"
	if _, err := run(t, eng, "do it"); err != nil {
		t.Fatal(err)
	}
	if got := eng.currentModel(); got != "fast-model" {
		t.Fatalf("currentModel() = %q after the fallback, want fast-model", got)
	}
	if got, _ := eng.turnModelSnap.Load().(string); got != "fast-model" {
		t.Fatalf("turnModelSnap = %q after the fallback, want fast-model", got)
	}
}

// ---------------------------------------------------------------------------
// 5. The tool-failure breaker starts each request afresh.
// ---------------------------------------------------------------------------

// consecutiveErrors was never reset between turns: after a bad previous turn
// the first failed batch of a new request tripped "3+ tool calls failed" and
// escalated at once.
func TestConsecutiveErrorsResetPerRequest(t *testing.T) {
	failing := &mockTool{name: "read", readOnly: true, safe: true, result: "Error: boom"}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "r1", Name: "read", Input: map[string]any{"input": "x"}}}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, failing)
	eng.consecutiveErrors = 2 // left over from an earlier turn
	if _, err := run(t, eng, "new request"); err != nil {
		t.Fatal(err)
	}
	for _, m := range eng.Messages() {
		if strings.Contains(m.Content, "tool calls all failed") {
			t.Fatal("one failed batch of a new request tripped the breaker")
		}
	}
	if eng.consecutiveErrors != 1 {
		t.Fatalf("consecutiveErrors = %d after one failed batch, want 1", eng.consecutiveErrors)
	}
}

// ---------------------------------------------------------------------------
// 6. The background review never reads the live session.
// ---------------------------------------------------------------------------

// The review runs after the part WaitBackground covers, so /new could enter a
// new session while applyReview read e.session for the skill's source_session.
// Run with -race: the read was unsynchronized.
func TestReviewDoesNotRaceWithNewSession(t *testing.T) {
	prov := &mockProvider{responses: append(workTurnResponses(), mockResponse{content: "SKILL: Race Flow | x | steps", delay: 50 * time.Millisecond})}
	eng := workReviewEngine(t, prov)
	eng.store = mustSessionStore(t)
	eng.enterSession(&session.Record{ID: "old-session", Title: "old"})
	armReview(eng)
	if _, err := run(t, eng, "question"); err != nil {
		t.Fatal(err)
	}
	// Returns once the waited part is done, while the review is still running.
	eng.NewSession(context.Background())
	eng.waitReview()
	data, err := os.ReadFile(autoSkillFile(t, "Race Flow"))
	if err != nil {
		t.Fatalf("skill file: %v", err)
	}
	if !strings.Contains(string(data), "source_session: old-session") {
		t.Fatalf("the skill names the wrong session:\n%s", data)
	}
}

// ---------------------------------------------------------------------------
// 7. Verify-gate output tails are cut at rune boundaries.
// ---------------------------------------------------------------------------

// truncateTail sliced bytes, so Chinese compiler output cut mid-rune went to
// the model as invalid UTF-8.
func TestTruncateTailKeepsValidUTF8(t *testing.T) {
	s := strings.Repeat("错", 100) // 300 bytes
	for _, n := range []int{1, 2, 3, 4, 5, 10, 299} {
		got := truncateTail(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateTail(%d) = %q, invalid UTF-8", n, got)
		}
		if !strings.HasPrefix(got, "... (truncated)\n") {
			t.Fatalf("truncateTail(%d) lacks the marker: %q", n, got)
		}
		if tail := strings.TrimPrefix(got, "... (truncated)\n"); len(tail) > n {
			t.Fatalf("truncateTail(%d) kept %d bytes", n, len(tail))
		}
	}
	if got := truncateTail("short", 10); got != "short" {
		t.Fatalf("short input changed: %q", got)
	}
}
