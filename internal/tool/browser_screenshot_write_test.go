package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

// The browser tool declared itself read-only while action=screenshot writes a
// PNG into the workspace. Read-only sub-agents get read-only tools with no
// permission gate, and plan mode lets read-only tools through, so both could
// overwrite project files. The Tool interface has no per-action read-only
// report, so the tool as a whole is not read-only.
func TestBrowserToolIsNotReadOnly(t *testing.T) {
	if NewBrowserTool().Def().IsReadOnly {
		t.Fatal("browser is declared read-only although screenshot writes a file")
	}
}

// A screenshot used to replace whatever regular file sat at its output path;
// "assets/logo.png" that was really a text file, or any project asset, was
// gone. Like draw_image, an existing file is replaced only if it is a PNG.
func TestBrowserScreenshotRefusesToOverwriteNonPNG(t *testing.T) {
	cwd := t.TempDir()
	bt := NewBrowserTool()
	tctx := Context{Cwd: cwd}

	if err := os.WriteFile(filepath.Join(cwd, "notes.png"), []byte("not an image\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{"action": "screenshot", "url": "https://8.8.8.8/", "output": "notes.png"}
	if d := bt.CheckPermissions(in, tctx); d.Decision != Deny {
		t.Errorf("non-PNG output: decision = %v (%s), want Deny", d.Decision, d.Reason)
	}
	res, err := bt.Call(context.Background(), in, tctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Data, "not a PNG") {
		t.Errorf("non-PNG output: Call = %q, want a refusal", res.Data)
	}
	if data, _ := os.ReadFile(filepath.Join(cwd, "notes.png")); string(data) != "not an image\n" {
		t.Errorf("notes.png was changed: %q", data)
	}

	if err := os.WriteFile(filepath.Join(cwd, "old.png"), append(append([]byte{}, pngSignature...), 0, 0, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	in = Input{"action": "screenshot", "url": "https://8.8.8.8/", "output": "old.png"}
	if d := bt.CheckPermissions(in, tctx); d.Decision != Ask {
		t.Errorf("existing PNG output: decision = %v (%s), want Ask", d.Decision, d.Reason)
	}
}

// The PNG went to disk with os.WriteFile, which truncates first: a crash or a
// full disk left an empty file, and none of write's atomic replace applied.
func TestSaveScreenshotWritesAtomically(t *testing.T) {
	cwd := t.TempDir()
	out := filepath.Join(cwd, "shots", "page.png")
	data := append(append([]byte{}, pngSignature...), 1, 2, 3)
	if err := saveScreenshot(out, data); err != nil {
		t.Fatalf("saveScreenshot: %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != string(data) {
		t.Fatalf("content = %q, want %q", got, data)
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fsatomic.IsTempName(e.Name()) {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}
