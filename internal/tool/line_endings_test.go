package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const bom = "\ufeff"

// The read tool shows lines without their \r, so the model writes LF text. On a
// CRLF file that produced mixed line endings in the edited region.
func TestEditKeepsCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.cs")
	if err := os.WriteFile(path, []byte("line1\r\nline2\r\nline3\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := NewEditTool().Call(context.Background(), Input{
		"filePath": path, "oldString": "line1\nline2", "newString": "LINE1\nLINE2\nNEW",
	}, Context{Cwd: dir})
	if res.IsError {
		t.Fatal(res.Data)
	}
	got, _ := os.ReadFile(path)
	if want := "LINE1\r\nLINE2\r\nNEW\r\nline3\r\n"; string(got) != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

// Overwriting a CRLF file with BOM from LF content turned every line into a
// diff and dropped the BOM some Windows tools need.
func TestWriteKeepsExistingLineEndingsAndBOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "App.config")
	if err := os.WriteFile(path, []byte(bom+"<a>\r\n</a>\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := NewWriteTool().Call(context.Background(), Input{"filePath": path, "content": "<a>\n  <b/>\n</a>\n"}, Context{Cwd: dir})
	if res.IsError {
		t.Fatal(res.Data)
	}
	got, _ := os.ReadFile(path)
	if want := bom + "<a>\r\n  <b/>\r\n</a>\r\n"; string(got) != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestWriteNewFileKeepsContentAsGiven(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.go")
	NewWriteTool().Call(context.Background(), Input{"filePath": path, "content": "package x\n"}, Context{Cwd: dir})
	if got, _ := os.ReadFile(path); string(got) != "package x\n" {
		t.Fatalf("file = %q", got)
	}
}
