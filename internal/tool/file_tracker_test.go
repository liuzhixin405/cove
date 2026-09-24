package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// guardedCtx is a tool context with a session Runtime, which turns on the
// read-before-write guard (it is skipped when Runtime is nil).
func guardedCtx(dir string) Context {
	return Context{Cwd: dir, Runtime: &Runtime{}}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func callTool(t *testing.T, tl Tool, input Input, tctx Context) Result {
	t.Helper()
	res, err := tl.Call(context.Background(), input, tctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestWriteRefusesExistingFileNeverRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	writeTestFile(t, path, "the user's notes\n")

	res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "replaced\n"}, guardedCtx(dir))
	if !res.IsError || !strings.Contains(res.Data, "Read it first") {
		t.Fatalf("write over an unread file = %q, want a read-it-first error", res.Data)
	}
	if got := readTestFile(t, path); got != "the user's notes\n" {
		t.Fatalf("file = %q, want it unchanged", got)
	}
}

func TestWriteCreatesNewFileWithoutRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "hello\n"}, guardedCtx(dir))
	if res.IsError {
		t.Fatal(res.Data)
	}
	if got := readTestFile(t, path); got != "hello\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditRefusesFileNeverRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTestFile(t, path, "x := 1\n")

	res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "x := 1", "newString": "x := 2"}, guardedCtx(dir))
	if !res.IsError || !strings.Contains(res.Data, "Read it first") {
		t.Fatalf("edit of an unread file = %q, want a read-it-first error", res.Data)
	}
	if got := readTestFile(t, path); got != "x := 1\n" {
		t.Fatalf("file = %q, want it unchanged", got)
	}
}

// The user (or a bash command) changed the file after the model read it; an
// edit or write based on the old view would silently discard that change.
func TestEditRefusesFileChangedSinceRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTestFile(t, path, "x := 1\n")
	tctx := guardedCtx(dir)
	if res := callTool(t, NewReadTool(), Input{"filePath": path}, tctx); res.IsError {
		t.Fatal(res.Data)
	}
	writeTestFile(t, path, "x := 1\ny := 3 // added by the user\n")

	res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "x := 1", "newString": "x := 2"}, tctx)
	if !res.IsError || !strings.Contains(res.Data, "changed since you last read it") {
		t.Fatalf("edit of a changed file = %q, want a changed-since-read error", res.Data)
	}
	if got := readTestFile(t, path); got != "x := 1\ny := 3 // added by the user\n" {
		t.Fatalf("file = %q, want the user's version untouched", got)
	}
}

func TestWriteRefusesFileChangedSinceRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeTestFile(t, path, "v1\n")
	tctx := guardedCtx(dir)
	callTool(t, NewReadTool(), Input{"filePath": path}, tctx)
	writeTestFile(t, path, "v2 from the user\n")

	res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "model version\n"}, tctx)
	if !res.IsError || !strings.Contains(res.Data, "changed since you last read it") {
		t.Fatalf("write over a changed file = %q, want a changed-since-read error", res.Data)
	}
	if got := readTestFile(t, path); got != "v2 from the user\n" {
		t.Fatalf("file = %q, want the user's version untouched", got)
	}
}

func TestEditAllowedAfterRereadingChangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTestFile(t, path, "x := 1\n")
	tctx := guardedCtx(dir)
	callTool(t, NewReadTool(), Input{"filePath": path}, tctx)
	writeTestFile(t, path, "x := 1\ny := 3\n")
	callTool(t, NewReadTool(), Input{"filePath": path}, tctx)

	res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "x := 1", "newString": "x := 2"}, tctx)
	if res.IsError {
		t.Fatal(res.Data)
	}
	if got := readTestFile(t, path); got != "x := 2\ny := 3\n" {
		t.Fatalf("file = %q", got)
	}
}

// cove's own writes update the record, so a run of edits does not need a
// read between each one.
func TestConsecutiveEditsNeedNoReread(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTestFile(t, path, "a := 1\nb := 1\n")
	tctx := guardedCtx(dir)
	callTool(t, NewReadTool(), Input{"filePath": path}, tctx)

	for _, e := range [][2]string{{"a := 1", "a := 2"}, {"b := 1", "b := 2"}} {
		res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": e[0], "newString": e[1]}, tctx)
		if res.IsError {
			t.Fatalf("edit %q: %s", e[0], res.Data)
		}
	}
	if got := readTestFile(t, path); got != "a := 2\nb := 2\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditAfterWriteNeedsNoRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	tctx := guardedCtx(dir)
	if res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "a := 1\n"}, tctx); res.IsError {
		t.Fatal(res.Data)
	}
	if res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "a := 1", "newString": "a := 2"}, tctx); res.IsError {
		t.Fatal(res.Data)
	}
	if res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "a := 3\n"}, tctx); res.IsError {
		t.Fatal(res.Data)
	}
}

// A partial read counts: edit only replaces text the model quotes, and big
// files cannot be read in one call at all.
func TestEditAllowedAfterPartialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	writeTestFile(t, path, sb.String())
	tctx := guardedCtx(dir)
	callTool(t, NewReadTool(), Input{"filePath": path, "offset": float64(40), "limit": float64(5)}, tctx)

	res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "line 42\n", "newString": "line forty-two\n"}, tctx)
	if res.IsError {
		t.Fatal(res.Data)
	}
	if got := readTestFile(t, path); !strings.Contains(got, "line forty-two\nline 43\n") || !strings.HasPrefix(got, "line 1\n") {
		t.Fatalf("file = %q", got)
	}
}

// A formatter, git checkout, or touch that rewrites identical bytes bumps the
// mtime without changing anything the model saw; that must not force a reread.
func TestEditAllowedWhenOnlyMtimeChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTestFile(t, path, "x := 1\n")
	tctx := guardedCtx(dir)
	callTool(t, NewReadTool(), Input{"filePath": path}, tctx)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "x := 1", "newString": "x := 2"}, tctx)
	if res.IsError {
		t.Fatal(res.Data)
	}
}

// read refuses to show UTF-16 and tells the model to convert the file; one way
// to do that is writing it again as UTF-8, which the guard must not block
// forever with "read it first".
func TestWriteAllowedOverUTF16FileAfterReadRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte{0xFF, 0xFE, 'h', 0, 'i', 0, '\r', 0, '\n', 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	tctx := guardedCtx(dir)
	if res := callTool(t, NewReadTool(), Input{"filePath": path}, tctx); !res.IsError {
		t.Fatalf("read of UTF-16 = %q, want the conversion error", res.Data)
	}

	res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "hi\n"}, tctx)
	if res.IsError {
		t.Fatal(res.Data)
	}
}

// Same for a minified file with a line too long for read: the model inspects
// it with bash, and edit must still be possible after the read attempt.
func TestEditAllowedAfterReadHitLongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.min.js")
	writeTestFile(t, path, "var a=1;"+strings.Repeat("x", 1100*1024)+"\n")
	tctx := guardedCtx(dir)
	if res := callTool(t, NewReadTool(), Input{"filePath": path}, tctx); !res.IsError {
		t.Fatal("read of a >1MB line succeeded, want the long-line error")
	}

	res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "var a=1;", "newString": "var a=2;"}, tctx)
	if res.IsError {
		t.Fatal(res.Data)
	}
}

// The model reads with one spelling of a path and edits with another.
func TestGuardMatchesRelativeAndAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTestFile(t, path, "x := 1\n")
	tctx := guardedCtx(dir)
	callTool(t, NewReadTool(), Input{"filePath": "a.go"}, tctx)

	res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "x := 1", "newString": "x := 2"}, tctx)
	if res.IsError {
		t.Fatal(res.Data)
	}
}

// Tool calls on different files run in parallel and share one Runtime (and so
// one tracker); run with -race.
func TestFileTrackerParallelUse(t *testing.T) {
	dir := t.TempDir()
	tctx := guardedCtx(dir)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
			if res := callTool(t, NewWriteTool(), Input{"filePath": path, "content": "v1\n"}, tctx); res.IsError {
				errs <- res.Data
				return
			}
			if res := callTool(t, NewReadTool(), Input{"filePath": path}, tctx); res.IsError {
				errs <- res.Data
				return
			}
			if res := callTool(t, NewEditTool(), Input{"filePath": path, "oldString": "v1", "newString": "v2"}, tctx); res.IsError {
				errs <- res.Data
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
