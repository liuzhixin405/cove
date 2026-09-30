//go:build windows

package tool

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// write/edit replace a file through fsatomic; a hidden file used to come back
// visible after every edit.
func TestReplaceFileKeepsHiddenAttribute(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(path)
	if err := windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(path, []byte("A=2\n")); err != nil {
		t.Fatal(err)
	}
	a, err := windows.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if a&windows.FILE_ATTRIBUTE_HIDDEN == 0 {
		t.Errorf("hidden attribute lost: %#x", a)
	}
	if b, _ := os.ReadFile(path); string(b) != "A=2\n" {
		t.Errorf("content = %q", b)
	}
}
