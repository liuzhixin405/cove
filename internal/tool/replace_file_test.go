package tool

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/fsatomic"
)

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fsatomic.IsTempName(e.Name()) {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

// statNow stats path and pins its file identity. On Windows os.SameFile looks
// the identity up by path when first asked, so a FileInfo taken before the
// write would otherwise report the file that is there afterwards.
func statNow(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	os.SameFile(info, info)
	return info
}

// os.WriteFile truncates the file and then writes it, so a crash or a full
// disk in between left the user with a truncated file. write and edit now put
// a new file in place, which shows as a different file identity afterwards.
func TestWriteReplacesFileInsteadOfTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeTestFile(t, path, "old\n")
	before := statNow(t, path)

	if res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "new\n"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	after := statNow(t, path)
	if os.SameFile(before, after) {
		t.Fatal("write rewrote the file in place, want an atomic replacement")
	}
	if got := readTestFile(t, path); got != "new\n" {
		t.Fatalf("file = %q", got)
	}
	assertNoTempFiles(t, dir)
}

func TestEditReplacesFileInsteadOfTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTestFile(t, path, "x := 1\n")
	before := statNow(t, path)

	if res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "x := 1", "newString": "x := 2"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	after := statNow(t, path)
	if os.SameFile(before, after) {
		t.Fatal("edit rewrote the file in place, want an atomic replacement")
	}
	if got := readTestFile(t, path); got != "x := 2\n" {
		t.Fatalf("file = %q", got)
	}
	assertNoTempFiles(t, dir)
}

// The replacement file starts out 0600; a script must stay executable.
func TestWriteKeepsPermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "run.sh")
	writeTestFile(t, path, "echo old\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	callTool(t, NewWriteTool(), Input{"filePath": path, "content": "echo new\n"}, Context{Cwd: dir})
	callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "new", "newString": "newer"}, Context{Cwd: dir})
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
}

// Writing through a symlink changes the file it points to; replacing the link
// itself with a regular file would silently unlink a shared config.
func TestWriteThroughSymlinkKeepsTheLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.conf")
	link := filepath.Join(dir, "app.conf")
	writeTestFile(t, target, "old\n")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if res := callTool(t, NewWriteTool(), Input{"filePath": link, "content": "new\n"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	if res := callTool(t, NewEditTool(), Input{"filePath": link, "oldString": "new", "newString": "newer"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced by a regular file (err %v)", err)
	}
	if got := readTestFile(t, target); got != "newer\n" {
		t.Fatalf("target = %q", got)
	}
}

// A dangling link inside the project can point anywhere, including outside it;
// the path check cannot resolve it, so write must not follow it.
func TestWriteRefusesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "planted.txt")
	link := filepath.Join(dir, "a.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	res := callTool(t, NewWriteTool(), Input{"filePath": link, "content": "x\n"}, Context{Cwd: dir})
	if !res.IsError || !strings.Contains(res.Data, "symbolic link") {
		t.Fatalf("write through a dangling link = %q, want it refused", res.Data)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("the link target was created")
	}
}

// On Windows a file another program holds open without FILE_SHARE_DELETE (Go's
// own os.Open, many editors and indexers) cannot be renamed over; the write
// must still go through rather than fail while the user has the file open.
func TestWriteSucceedsWhileFileIsHeldOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "open.txt")
	writeTestFile(t, path, "old\n")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "new\n"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	if res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "new", "newString": "newer"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	if got := readTestFile(t, path); got != "newer\n" {
		t.Fatalf("file = %q", got)
	}
	assertNoTempFiles(t, dir)
}

// Replacing a file breaks its hard links: the other names would keep the old
// content. A file with more than one link is written in place instead.
func TestWriteKeepsHardLinksShared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	other := filepath.Join(dir, "b.txt")
	writeTestFile(t, path, "old\n")
	if err := os.Link(path, other); err != nil {
		t.Skipf("hard link unavailable: %v", err)
	}

	if res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "new\n"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	if res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "new", "newString": "newer"}, Context{Cwd: dir}); res.IsError {
		t.Fatal(res.Data)
	}
	if got := readTestFile(t, other); got != "newer\n" {
		t.Fatalf("other link = %q, want the new content", got)
	}
}
