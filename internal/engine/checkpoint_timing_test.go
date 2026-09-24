package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/checkpoint"
	"github.com/liuzhixin405/cove/internal/tool"
)

// fileWriteTool is a "write" that really writes, immediately, to its path.
type fileWriteTool struct{ dir string }

func (t *fileWriteTool) Def() tool.Def {
	return tool.Def{
		Name:        "write",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`),
	}
}
func (t *fileWriteTool) Validate(tool.Input) string { return "" }
func (t *fileWriteTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Allow}
}
func (t *fileWriteTool) Call(_ context.Context, in tool.Input, _ tool.Context) (tool.Result, error) {
	p, _ := in["file_path"].(string)
	if err := os.WriteFile(filepath.Join(t.dir, p), []byte("after"), 0o600); err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	return tool.Result{Data: "wrote " + p}, nil
}

// The auto-checkpoint exists so /undo can take back a write. It used to be
// taken on a goroutine racing the write itself (and its parallel siblings),
// so the snapshot usually already contained the edit it was meant to undo.
func TestAutoCheckpointCapturesStateBeforeWrites(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{
			{ID: "w1", Name: "write", Input: map[string]any{"file_path": "a.txt"}},
			{ID: "w2", Name: "write", Input: map[string]any{"file_path": "b.txt"}},
		}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, &fileWriteTool{dir: dir})
	cp, err := checkpoint.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	eng.cpMgr = cp

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "edit both"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RestoreCheckpoint(""); err != nil {
		t.Fatalf("undo: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		if string(data) != "before" {
			t.Errorf("%s = %q after undo, want the pre-write content", name, data)
		}
	}
}
