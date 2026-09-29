package engine

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// outsidePathFor picks the path flavour this platform's filepath understands:
// outsideDirectoryOf is filepath.Dir over what the refusal names, and off
// Windows a "D:\..." path is one relative file name whose Dir is ".".
func outsidePathFor() (dir, file string) {
	if runtime.GOOS == "windows" {
		return `D:\github\agent`, `D:\github\agent\a.csproj`
	}
	return "/home/me/agent", "/home/me/agent/a.csproj"
}

// A tool refusing a path outside the working directory is a message for
// the person, not only for the model: a real session spent 90 minutes
// trying bash and powershell around the refusal while the user saw only
// "工具 write 失败". The engine says once per directory what to do.
func TestOutsideWorkingDirectoryRefusalTellsTheUserToChangeDirectory(t *testing.T) {
	dir, file := outsidePathFor()
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{
			{ID: "c1", Name: "write", Input: map[string]any{"filePath": file}},
			{ID: "c2", Name: "write", Input: map[string]any{"filePath": file}},
		}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, &mockTool{name: "write", safe: true, readOnly: true, result: `Error: path outside working directory: ` + file})
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: `在 ` + dir + ` 建项目`}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	hints := 0
	for _, l := range lines {
		if strings.Contains(l, "/cd") && strings.Contains(l, dir) {
			hints++
		}
	}
	if hints != 1 {
		t.Fatalf("want exactly one /cd hint naming the directory, got %d in:\n%s", hints, strings.Join(lines, "\n"))
	}
}

// outsideDirectoryOf extracts the directory a path refusal names.
func TestOutsideDirectoryOfParsesBothRefusals(t *testing.T) {
	cases := map[string]string{
		`Error: path outside working directory: /home/me/proj/src/a.go`: `/home/me/proj/src`,
		`Error: path outside working directory: /home/me/proj`:          `/home/me`,
		`Error: file not found: /home/me/x.go`:                          "",
	}
	if runtime.GOOS == "windows" {
		cases = map[string]string{
			`Error: path outside working directory: D:\github\agent\src\a.csproj`: `D:\github\agent\src`,
			// The cross-drive refusal is Windows-only: only there can a path be
			// on a different volume from the working directory.
			`Error: path on different drive: C:\Users\me\x.bat (cwd is on D:)`: `C:\Users\me`,
			`Error: path outside working directory: D:\github\agent`:           `D:\github`,
			`Error: file not found: D:\x.go`:                                   "",
		}
	}
	for in, want := range cases {
		if got := outsideDirectoryOf(in); got != want {
			t.Errorf("outsideDirectoryOf(%q) = %q, want %q", in, got, want)
		}
	}
}
