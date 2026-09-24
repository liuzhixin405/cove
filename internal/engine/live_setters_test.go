package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/session"
)

func isolatedHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// /system used to replace the whole system prompt (role, tool rules, project
// context). It sets the user's instructions, which are appended, and takes
// effect in the running session.
func TestSetCustomInstructionsAppendsToThePrompt(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	base := eng.SystemPrompt()
	eng.SetCustomInstructions("所有回答使用中文。")
	got := eng.SystemPrompt()
	if !strings.Contains(got, "所有回答使用中文。") {
		t.Fatal("instructions missing from the system prompt")
	}
	if !strings.HasPrefix(got, strings.TrimSpace(base)[:40]) {
		t.Fatal("the base system prompt was replaced instead of extended")
	}
}

// After /cd the session belongs to the new project, and /undo snapshots and
// restores the new directory, not the one cove was started in.
func TestSetWorkingDirMovesSessionAndCheckpoints(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolatedHome(t)
	start, next := t.TempDir(), t.TempDir()
	t.Chdir(start)
	if err := os.WriteFile(filepath.Join(next, "a.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "w1", Name: "write", Input: map[string]any{"file_path": "a.txt"}}}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, &fileWriteTool{dir: next})

	t.Chdir(next) // what /cd does before telling the engine
	eng.SetWorkingDir(next)

	if want := session.NormalizeProjectDir(next); eng.Session().Cwd != want {
		t.Fatalf("session Cwd = %q, want %q", eng.Session().Cwd, want)
	}
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "edit"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RestoreCheckpoint(""); err != nil {
		t.Fatalf("undo in the new directory: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(next, "a.txt")); string(data) != "before" {
		t.Fatalf("a.txt = %q after undo, want the pre-write content", data)
	}
}

// Resuming a session continued it under a brand-new ID, so every resume left
// a copy behind and the original never received the new turns.
func TestResumeSessionKeepsTheSessionID(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "continued"}}})
	if eng.store == nil {
		t.Skip("engine has no session store")
	}
	old := &session.Record{
		ID:       "session-original",
		Title:    "parser work",
		Model:    "deepseek-v4-pro",
		Messages: []api.Message{{Role: "user", Content: "fix the parser"}, {Role: "assistant", Content: "fixed"}},
	}
	eng.ResumeSession(old)
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "and the tests?"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	rec, err := eng.store.Load("session-original")
	if err != nil {
		t.Fatalf("original session not updated: %v", err)
	}
	if n := len(rec.Messages); n < 4 {
		t.Fatalf("original session has %d messages after resuming, want the new turn appended", n)
	}
}

// Session IDs were Unix seconds: two `cove -p` runs started in the same
// second wrote the same session file.
func TestSessionIDsAreUniqueWithinASecond(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newSessionID()
		if seen[id] {
			t.Fatalf("session ID %q generated twice", id)
		}
		seen[id] = true
	}
}

func TestResumeSessionIgnoresNil(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	before := eng.Session()
	eng.ResumeSession(nil)
	if eng.Session() != before {
		t.Fatal("resuming nil replaced the current session")
	}
}
