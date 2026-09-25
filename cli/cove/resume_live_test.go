package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/checkpoint"
	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/session"
	"github.com/liuzhixin405/cove/internal/state"
)

// savedSession writes a two-message session with a first engine and returns
// its ID; the test then resumes it in a second engine.
func savedSession(t *testing.T) string {
	t.Helper()
	first := newTestEngine(t)
	first.LoadMessages([]api.Message{{Role: "user", Content: "修复登录 bug"}, {Role: "assistant", Content: "好的"}})
	first.SaveSession()
	return first.SessionID()
}

func secondEngine(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		Model:    "deepseek-v4-pro",
		Provider: api.ProviderConfig{Name: "deepseek", APIKey: "placeholder", BaseURL: "http://127.0.0.1:1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// assertContinuesSession checks that eng carries on under id: resuming used
// to load the messages into a fresh session, so every resume saved a copy
// under a new ID and the original never grew.
func assertContinuesSession(t *testing.T, eng *engine.Engine, id string) {
	t.Helper()
	if eng.SessionID() != id {
		t.Fatalf("session ID after resume = %q, want %q", eng.SessionID(), id)
	}
	eng.LoadMessages(append(eng.Messages(), api.Message{Role: "user", Content: "继续"}))
	eng.SaveSession()
	r, err := eng.Store().Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 3 {
		t.Fatalf("saved session %s has %d messages, want 3", id, len(r.Messages))
	}
	home, _ := os.UserHomeDir()
	files, _ := session.ListSessionFiles(filepath.Join(home, ".cove", "sessions"))
	if len(files) != 1 || files[0] != id+".jsonl" {
		t.Fatalf("session files = %v, want only the resumed one", files)
	}
}

func TestStartupResumeContinuesSession(t *testing.T) {
	id := savedSession(t)
	eng := secondEngine(t)
	if _, _, err := resumeStartupSession(eng.Store(), id, currentProjectDir(), eng.ResumeSession); err != nil {
		t.Fatal(err)
	}
	assertContinuesSession(t, eng, id)
}

func TestResumeCommandContinuesSession(t *testing.T) {
	id := savedSession(t)
	eng := secondEngine(t)
	captureOut(t)
	handleResume(context.Background(), id, eng)
	assertContinuesSession(t, eng, id)
}

func TestHistoryResumeContinuesSession(t *testing.T) {
	id := savedSession(t)
	eng := secondEngine(t)
	captureOut(t)
	handleHistoryResumeIn("1", eng, false)
	assertContinuesSession(t, eng, id)
}

func TestContinueResumeContinuesSession(t *testing.T) {
	id := savedSession(t)
	eng := secondEngine(t)
	captureOut(t)
	if !handleHistoryResumeMostRelevant(eng) {
		t.Fatal("no session resumed")
	}
	assertContinuesSession(t, eng, id)
}

// The registered /resume command (used where the built-in one is not) goes
// through the adapter.
func TestResumeCmdThroughAdapterContinuesSession(t *testing.T) {
	id := savedSession(t)
	eng := secondEngine(t)
	_, err := command.NewResumeCmd().Execute(context.Background(), command.Input{
		Args: []string{id}, Cwd: currentProjectDir(), SessionStore: eng.Store(),
		Engine: replEngineAdapter{eng: eng}, AppState: &state.AppState{},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertContinuesSession(t, eng, id)
}

func TestSystemCmdAppliesLiveThroughAdapter(t *testing.T) {
	eng := newTestEngine(t)
	out, err := command.NewSystemCmd().Execute(context.Background(), command.Input{
		Args: []string{"始终用中文回答"}, Config: config.DefaultConfig(),
		SaveConfig: func(*config.Config) error { return nil }, Engine: replEngineAdapter{eng: eng},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Message, "下次") {
		t.Fatalf("/system still defers to the next start: %q", out.Message)
	}
	prompt := eng.SystemPrompt()
	if !strings.Contains(prompt, "始终用中文回答") || !strings.Contains(prompt, "# Role") {
		t.Fatal("system prompt must keep the built-in prompt and add the instructions")
	}
}

// After /cd, /undo must restore the new directory's checkpoint, not the
// directory cove was started in.
func TestCdMovesUndoToNewDirectory(t *testing.T) {
	eng := newTestEngine(t)
	a := replEngineAdapter{eng: eng}
	target := t.TempDir()
	restore, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(restore) })
	file := filepath.Join(target, "a.txt")
	if err := os.WriteFile(file, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := command.NewCdCmd().Execute(context.Background(), command.Input{Args: []string{target}, Cwd: restore, Engine: a})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Message, "注意") {
		t.Fatalf("/cd still says the engine stays behind: %q", out.Message)
	}

	wd, _ := os.Getwd()
	cp, err := checkpoint.New(wd)
	if err != nil {
		t.Skipf("checkpoints unavailable here: %v", err)
	}
	if _, err := cp.Create("before edit"); err != nil {
		t.Skipf("checkpoints unavailable here: %v", err)
	}
	if err := os.WriteFile(file, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err = command.NewUndoCmd().Execute(context.Background(), command.Input{Engine: a})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if strings.TrimSpace(string(data)) != "v1" {
		t.Fatalf("after /undo a.txt = %q (%s), want v1", data, out.Message)
	}
}
