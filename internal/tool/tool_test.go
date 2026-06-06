package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTool(t *testing.T) {
	tr := NewReadTool()
	if tr.Def().Name != "read" {
		t.Errorf("expected name 'read', got %q", tr.Def().Name)
	}
	if !tr.Def().IsReadOnly {
		t.Error("read tool should be read-only")
	}

	result, _ := tr.Call(context.Background(), Input{"filePath": "/nonexistent"}, Context{})
	if !result.IsError {
		t.Error("expected error for nonexistent file")
	}
}

func TestWriteTool(t *testing.T) {
	tw := NewWriteTool()
	if tw.Def().Name != "write" {
		t.Errorf("expected name 'write', got %q", tw.Def().Name)
	}

	perm := tw.CheckPermissions(nil, Context{PermissionMode: "auto"})
	if perm.Decision != Allow {
		t.Errorf("expected Allow in auto mode, got %v", perm.Decision)
	}

	perm = tw.CheckPermissions(nil, Context{PermissionMode: "plan"})
	if perm.Decision != Deny {
		t.Errorf("expected Deny in plan mode, got %v", perm.Decision)
	}
}

func TestReadWriteToolsStayWithinCwd(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	read := NewReadTool()
	result, _ := read.Call(context.Background(), Input{"filePath": outsideFile}, Context{Cwd: root})
	if !result.IsError || !strings.Contains(result.Data, "outside working directory") {
		t.Fatalf("expected outside read to be denied, got %+v", result)
	}

	write := NewWriteTool()
	result, _ = write.Call(context.Background(), Input{"filePath": filepath.Join(outside, "out.txt"), "content": "data"}, Context{Cwd: root})
	if !result.IsError || !strings.Contains(result.Data, "outside working directory") {
		t.Fatalf("expected outside write to be denied, got %+v", result)
	}
}

func TestWriteToolDeniesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	write := NewWriteTool()
	result, _ := write.Call(context.Background(), Input{"filePath": filepath.Join(link, "nested", "out.txt"), "content": "data"}, Context{Cwd: root})
	if !result.IsError || !strings.Contains(result.Data, "outside working directory") {
		t.Fatalf("expected symlink escape write to be denied, got %+v", result)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(NewReadTool())
	r.Register(NewWriteTool())

	if len(r.All()) != 2 {
		t.Errorf("expected 2 tools, got %d", len(r.All()))
	}

	tool, ok := r.Find("read")
	if !ok || tool.Def().Name != "read" {
		t.Error("read tool not found")
	}

	_, ok = r.Find("nonexistent")
	if ok {
		t.Error("should not find nonexistent tool")
	}
}

func TestTaskTools(t *testing.T) {
	rt := &Runtime{Tasks: make(map[string]*TaskRecord)}
	ctx := Context{Runtime: rt}

	tc := NewTaskCreateTool()
	result, _ := tc.Call(context.Background(), Input{"title": "test", "description": "test desc"}, ctx)
	if result.IsError {
		t.Errorf("task create failed: %s", result.Data)
	}

	if len(rt.Tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(rt.Tasks))
	}

	tl := NewTaskListTool()
	result, _ = tl.Call(context.Background(), nil, ctx)
	if result.Data == "" {
		t.Error("task list returned empty")
	}
}

func TestPermissionDecision(t *testing.T) {
	if Allowed("test").Decision != Allow {
		t.Error("Allowed() should return Allow")
	}
	if Denied("test").Decision != Deny {
		t.Error("Denied() should return Deny")
	}
	if Asked("test").Decision != Ask {
		t.Error("Asked() should return Ask")
	}
}
