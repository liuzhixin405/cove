package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/termui"
)

// Commands that rewrite engine state must not run while a task goroutine is
// appending to the same history: /compact mid-turn split a tool round and
// the next request was rejected.
func TestCommandMutatesEngine(t *testing.T) {
	for _, in := range []string{"/compact", "/cd ..", "/history 3", "/resume abc", "/model gpt-x", "/provider openai", "/api-key sk", "/profile switch p"} {
		if !commandMutatesEngine(in) {
			t.Errorf("%q should be blocked while a task runs", in)
		}
	}
	for _, in := range []string{"/history", "/tasks", "/help", "/status", "/stop", "hello", "/profile", "/diagnose errors"} {
		if commandMutatesEngine(in) {
			t.Errorf("%q should stay allowed while a task runs", in)
		}
	}
}

// Only the letters the prompt offers count as answers; anything else is the
// person's next instruction and must not be swallowed as "n".
func TestPermissionAnswerAccepted(t *testing.T) {
	for _, in := range []string{"y", "Y", "yes", "是", "允许", "a", "always", "总是", "p", "永久", "n", "no", "否", "拒绝", " n "} {
		if !permissionAnswerAccepted(in) {
			t.Errorf("%q should be an answer", in)
		}
	}
	for _, in := range []string{"", "请把测试也改掉", "/stop", "exit", "yeah", "nope"} {
		if permissionAnswerAccepted(in) {
			t.Errorf("%q should not be an answer", in)
		}
	}
}

// Denying prints a line: the result used to reach only the model.
func TestDeniedAnswerPrintsFeedback(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "rm -rf build"}, "n")
	if allow {
		t.Fatal("answer n must deny")
	}
	if !strings.Contains(out, "已拒绝 bash") {
		t.Errorf("no denial line:\n%s", out)
	}
	if !strings.Contains(out, "Ctrl+C") {
		t.Errorf("options do not say what Ctrl+C does:\n%s", out)
	}
}

// -p and headless runs get the engine's lines (compaction, blocked tools,
// stall) on stderr, and termui's output there too, so stdout stays the
// answer alone.
func TestNonInteractiveOutputGoesToStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	t.Chdir(t.TempDir())
	t.Cleanup(func() { termui.SetWriter(nil) })
	eng, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	wireNonInteractiveOutput(eng)
	if eng.OnEngineOutput == nil {
		t.Fatal("engine output not wired")
	}
	// termui's writer stays on stdout: the answer is printed through it (see
	// TestE2E_PrintModeKeepsStdoutForTheAnswer).
	if termui.Writer() != os.Stdout {
		t.Fatal("termui was redirected away from stdout, which carries the answer")
	}
}

// The manual says a timed-out request is not retried (it may have been
// billed); the REPL's retry list disagreed.
func TestTransientRetryExcludesTimeouts(t *testing.T) {
	for _, s := range []string{"Post http://x: timeout awaiting response headers", "context deadline exceeded", "request timed out"} {
		if isTransientRequestError(errors.New(s)) {
			t.Errorf("%q retried although timeouts are not to be", s)
		}
	}
	for _, s := range []string{"read tcp: connection reset by peer", "server error 503", "unexpected EOF"} {
		if !isTransientRequestError(errors.New(s)) {
			t.Errorf("%q not retried", s)
		}
	}
}

// A one-word input is not "the same task" as a queued sentence that happens
// to contain the word; merging dropped it while claiming it was added.
func TestShortInputIsNotMergedIntoQueuedTask(t *testing.T) {
	queued := api.Message{Role: "user", Content: "run pytest and fix the failures"}
	for _, short := range []string{"y", "run", "fix it"} {
		if canMergeQueuedTask(queued, api.Message{Role: "user", Content: short}) {
			t.Errorf("%q merged into %q", short, queued.Content)
		}
	}
	if !canMergeQueuedTask(queued, api.Message{Role: "user", Content: "run pytest and fix the failures in the parser"}) {
		t.Error("a genuinely overlapping request was not merged")
	}
}

// Finishing a task releases its context.
func TestFinishLockedCancelsTheTaskContext(t *testing.T) {
	r := newREPLTaskRunner(nil)
	cancelled := false
	r.mu.Lock()
	r.running = true
	r.cancel = func() { cancelled = true }
	r.finishLocked()
	r.mu.Unlock()
	if !cancelled {
		t.Fatal("finishLocked leaked the task's context")
	}
}
