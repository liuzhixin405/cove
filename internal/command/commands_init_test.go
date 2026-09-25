package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeGuideEngine struct {
	EngineView
	reply  string
	err    error
	prompt string
}

func (f *fakeGuideEngine) GenerateOnce(_ context.Context, system, prompt string) (string, error) {
	f.prompt = prompt
	return f.reply, f.err
}

func initEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// /init drafts CLAUDE.md with the model, shows it as a diff, and writes it
// only on /init apply.
func TestInitDraftsWithModelAndAppliesOnConfirm(t *testing.T) {
	dir := initEnv(t)
	eng := &fakeGuideEngine{reply: "# Guide\n\nRun go test ./...\n"}
	out, err := NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("CLAUDE.md written before confirmation")
	}
	if !strings.Contains(out.Message, "+ Run go test ./...") || !strings.Contains(out.Message, "/init apply") {
		t.Fatalf("draft diff / instructions missing:\n%s", out.Message)
	}
	if !strings.Contains(eng.prompt, "module example.com/x") {
		t.Fatal("model did not get the repository digest")
	}
	out, err = NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Args: []string{"apply"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil || string(data) != "# Guide\n\nRun go test ./...\n" {
		t.Fatalf("applied content = %q, %v (%s)", data, err, out.Message)
	}
	// Draft is consumed.
	out, _ = NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Args: []string{"apply"}})
	if !strings.Contains(out.Message, "没有") {
		t.Fatalf("second apply: %q", out.Message)
	}
}

func TestInitDiscardDropsDraft(t *testing.T) {
	dir := initEnv(t)
	eng := &fakeGuideEngine{reply: "# G\n"}
	if _, err := NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Engine: eng}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Args: []string{"discard"}}); err != nil {
		t.Fatal(err)
	}
	out, _ := NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Args: []string{"apply"}})
	if !strings.Contains(out.Message, "没有") {
		t.Fatalf("apply after discard: %q", out.Message)
	}
}

// Without a model (or when the call fails) /init falls back to the template,
// still as a draft to confirm.
func TestInitFallsBackToTemplate(t *testing.T) {
	dir := initEnv(t)
	out, err := NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Engine: &fakeGuideEngine{err: errors.New("offline")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "+ go build ./...") || !strings.Contains(out.Message, "模板") {
		t.Fatalf("template fallback: %s", out.Message)
	}
}

func TestInitNotesAgentsMD(t *testing.T) {
	dir := initEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Engine: &fakeGuideEngine{reply: "# G\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "已检测到 AGENTS.md，将同时加载") || strings.Contains(out.Message, "缺少") {
		t.Fatalf("AGENTS.md note: %s", out.Message)
	}
}

func TestInitExistingClaudeMD(t *testing.T) {
	dir := initEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := NewInitCmd().Execute(context.Background(), Input{Cwd: dir, Engine: &fakeGuideEngine{reply: "# G\n"}})
	if !strings.Contains(out.Message, "已存在") {
		t.Fatalf("existing: %s", out.Message)
	}
}
