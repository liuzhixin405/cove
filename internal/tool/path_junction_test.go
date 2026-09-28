package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The file tools' sandbox resolved links with filepath.EvalSymlinks, which
// since Go 1.23 leaves Windows junctions unresolved: a junction inside the
// working directory pointing elsewhere let write out of it.
func TestWriteThroughALinkOutOfTheWorkingDirectoryIsRefused(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(cwd, "escape")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Skipf("mklink /J failed: %v %s", err, out)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	res, _ := NewWriteTool().Call(context.Background(),
		Input{"filePath": filepath.Join(link, "pwned.txt"), "content": "x"},
		Context{Cwd: cwd, PermissionMode: "bypass"})
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatalf("write escaped the working directory through a link: %s", res.Data)
	}
	if !res.IsError {
		t.Fatalf("write through the link was not refused: %s", res.Data)
	}

	// An ordinary new file inside the working directory still works.
	res, _ = NewWriteTool().Call(context.Background(),
		Input{"filePath": filepath.Join(cwd, "sub", "ok.txt"), "content": "x"},
		Context{Cwd: cwd, PermissionMode: "bypass"})
	if res.IsError {
		t.Fatalf("an in-project write was refused: %s", res.Data)
	}
}
