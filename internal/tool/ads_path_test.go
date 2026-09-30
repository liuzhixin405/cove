package tool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// On Windows "main.go:x.png" names an alternate data stream of main.go: the
// .png check passed and the screenshot was written into the source file's
// hidden stream.
func TestScreenshotPathRefusesAlternateDataStreams(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("alternate data streams are an NTFS feature")
	}
	cwd := t.TempDir()
	for _, out := range []string{"main.go:x.png", `sub\main.go:x.png`, filepath.Join(cwd, "main.go:x.png"), "shot.png::$DATA"} {
		if p, err := screenshotPath(Input{"output": out}, cwd); err == nil {
			t.Errorf("output %q accepted: %s", out, p)
		}
	}
	// A drive-letter volume is not a stream.
	if _, err := screenshotPath(Input{"output": filepath.Join(cwd, "shots", "x.png")}, cwd); err != nil {
		t.Errorf("absolute path refused: %v", err)
	}
}

func TestDrawImageRefusesAlternateDataStreams(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("alternate data streams are an NTFS feature")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewDrawImageTool().Call(context.Background(), Input{
		"outputPath": "main.go:x.png", "width": 4.0, "height": 4.0,
		"shapes": []any{map[string]any{"type": "fill"}},
	}, Context{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("stream path accepted: %s", res.Data)
	}
	if _, err := os.Stat(src + ":x.png"); err == nil {
		t.Error("stream written")
	}
}

func TestHasStreamSeparator(t *testing.T) {
	cases := map[string]bool{
		`C:\ws\a.png`:         false,
		`C:\ws\main.go:x.png`: true,
		`\server\share\a.png`: false,
		`\server\share\a:b`:   true,
		`a.png`:               false,
		`a.png:s`:             true,
		`C:a.png`:             false,
		`\\?\C:\ws\a.png`:     false,
		`\\?\C:\ws\a.png:s`:   true,
	}
	for p, want := range cases {
		if got := hasStreamSeparator(p, "windows"); got != want {
			t.Errorf("hasStreamSeparator(%q) = %v, want %v", p, got, want)
		}
	}
	if hasStreamSeparator("dir/a:b.png", "linux") {
		t.Error("':' is an ordinary file name character off Windows")
	}
}

// write and edit went through resolvePathInCwd, which did not check for a
// stream separator: write {filePath:"README.md:notes"} created a hidden NTFS
// alternate data stream of README.md inside the workspace, invisible to git,
// Explorer and read. Every file tool refuses such a path now.
func TestFileToolsRefuseAlternateDataStreams(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("alternate data streams are an NTFS feature")
	}
	dir := t.TempDir()
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tctx := Context{Cwd: dir}
	ctx := context.Background()
	calls := map[string]func() (Result, error){
		"write": func() (Result, error) {
			return NewWriteTool().Call(ctx, Input{"filePath": "README.md:notes", "content": "secret"}, tctx)
		},
		"edit": func() (Result, error) {
			return NewEditTool().Call(ctx, Input{"filePath": readme + ":notes", "oldString": "a", "newString": "b"}, tctx)
		},
		"read": func() (Result, error) {
			return NewReadTool().Call(ctx, Input{"filePath": "README.md:notes"}, tctx)
		},
	}
	for name, call := range calls {
		res, err := call()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !res.IsError || !strings.Contains(res.Data, "alternate data stream") {
			t.Errorf("%s accepted a stream path: %q", name, res.Data)
		}
	}
	if _, err := os.Stat(readme + ":notes"); err == nil {
		t.Error("stream README.md:notes was written")
	}
	if data, _ := os.ReadFile(readme); string(data) != "# hi\n" {
		t.Errorf("README.md changed: %q", data)
	}
}
