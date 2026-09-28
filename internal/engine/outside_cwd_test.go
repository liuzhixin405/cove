package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// A tool refusing a path outside the working directory is a message for
// the person, not only for the model: a real session spent 90 minutes
// trying bash and powershell around the refusal while the user saw only
// "工具 write 失败". The engine says once per directory what to do.
func TestOutsideWorkingDirectoryRefusalTellsTheUserToChangeDirectory(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{
			{ID: "c1", Name: "write", Input: map[string]any{"filePath": `D:\github\agent\a.csproj`}},
			{ID: "c2", Name: "write", Input: map[string]any{"filePath": `D:\github\agent\b.csproj`}},
		}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, &mockTool{name: "write", safe: true, readOnly: true, result: `Error: path outside working directory: D:\github\agent\a.csproj`})
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: `在 D:\github\agent 建项目`}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	hints := 0
	for _, l := range lines {
		if strings.Contains(l, "/cd") && strings.Contains(l, `D:\github\agent`) {
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
		`Error: path outside working directory: D:\github\agent\src\a.csproj`: `D:\github\agent\src`,
		`Error: path on different drive: C:\Users\me\x.bat (cwd is on D:)`:    `C:\Users\me`,
		`Error: path outside working directory: D:\github\agent`:              `D:\github`,
		`Error: file not found: D:\x.go`:                                      "",
	}
	for in, want := range cases {
		if got := outsideDirectoryOf(in); got != want {
			t.Errorf("outsideDirectoryOf(%q) = %q, want %q", in, got, want)
		}
	}
}
