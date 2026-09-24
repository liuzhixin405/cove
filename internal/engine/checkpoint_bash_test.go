package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/checkpoint"
	"github.com/liuzhixin405/cove/internal/tool"
)

// fakeShellTool is a "bash" whose commands are interpreted in-process: "rm X"
// deletes X, anything else only reads.
type fakeShellTool struct{ dir string }

func (t *fakeShellTool) Def() tool.Def {
	return tool.Def{Name: "bash", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}
}
func (t *fakeShellTool) Validate(tool.Input) string { return "" }
func (t *fakeShellTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Allow}
}
func (t *fakeShellTool) Call(_ context.Context, in tool.Input, _ tool.Context) (tool.Result, error) {
	cmd, _ := in["command"].(string)
	if name, ok := strings.CutPrefix(cmd, "rm "); ok {
		if err := os.Remove(filepath.Join(t.dir, name)); err != nil {
			return tool.Result{Data: err.Error(), IsError: true}, nil
		}
	}
	return tool.Result{Data: "ok"}, nil
}

func runShellTurn(t *testing.T, command string) (*Engine, *checkpoint.Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "s1", Name: "bash", Input: map[string]any{"command": command}}}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, &fakeShellTool{dir: dir})
	cp, err := checkpoint.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	eng.cpMgr = cp
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "go"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	return eng, cp, dir
}

// A file deleted or rewritten by a shell command (rm, sed -i, a generator)
// could not be undone: checkpoints were only taken before write/edit.
func TestShellCommandThatMayWriteIsCheckpointed(t *testing.T) {
	eng, _, dir := runShellTurn(t, "rm main.go")
	if _, err := eng.RestoreCheckpoint(""); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Fatal("main.go deleted by a shell command was not restored by undo")
	}
}

// Read-only commands are the bulk of shell calls; snapshotting before each
// one would cost a git add of the whole tree for nothing.
func TestReadOnlyShellCommandIsNotCheckpointed(t *testing.T) {
	_, cp, _ := runShellTurn(t, "git status")
	if got := cp.List(); len(got) != 0 {
		t.Fatalf("read-only command created checkpoints: %q", got)
	}
}

// namedWriter is a tool that writes a file when called, under any name.
type namedWriter struct {
	fileWriteTool
	name string
}

func (t *namedWriter) Def() tool.Def {
	d := t.fileWriteTool.Def()
	d.Name = t.name
	return d
}

// A sub-agent's writes run with the delegating call's context, which is
// already marked as checkpointed, so they skipped the snapshot — and the
// delegating call itself (agent, execute_plan, team_create) never triggered
// one. /undo could not take back anything a sub-agent wrote.
func TestDelegatingCallIsCheckpointed(t *testing.T) {
	for _, name := range []string{"agent", "execute_plan", "team_create"} {
		t.Run(name, func(t *testing.T) {
			if _, err := exec.LookPath("git"); err != nil {
				t.Skip("git not available")
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			prov := &mockProvider{responses: []mockResponse{
				{toolCalls: []api.ToolCall{{ID: "d1", Name: name, Input: map[string]any{"file_path": "a.txt"}}}},
				{content: "done"},
			}}
			eng := newTestEngine(prov, &namedWriter{fileWriteTool{dir: dir}, name})
			cp, err := checkpoint.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			eng.cpMgr = cp
			if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "delegate"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.RestoreCheckpoint(""); err != nil {
				t.Fatalf("undo: %v", err)
			}
			if data, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(data) != "before" {
				t.Fatalf("a.txt = %q after undo, want the content from before the delegation", data)
			}
		})
	}
}
