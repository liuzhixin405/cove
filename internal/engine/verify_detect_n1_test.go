package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The automatic completion check covers .NET, Node build scripts and Python.
func TestDetectVerifyCommandsMoreEcosystems(t *testing.T) {
	python := pythonCommand()
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"sln", map[string]string{"App.sln": "x"}, []string{"dotnet build --nologo -v q"}},
		{"csproj", map[string]string{"App.csproj": "<Project/>"}, []string{"dotnet build --nologo -v q"}},
		{"two slns", map[string]string{"A.sln": "x", "B.sln": "x"}, nil},
		{"npm build", map[string]string{"package.json": `{"scripts":{"build":"vite build"}}`}, []string{"npm run build --if-present"}},
		{"npm no build", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`}, nil},
		{"npm broken json", map[string]string{"package.json": `{`}, nil},
		{"pyproject", map[string]string{"pyproject.toml": "[project]"}, []string{python + ` -m compileall -q -x "(\.venv|venv|node_modules|\.git|__pycache__)" .`}},
		{"setup.py", map[string]string{"setup.py": "from setuptools import setup"}, []string{python + ` -m compileall -q -x "(\.venv|venv|node_modules|\.git|__pycache__)" .`}},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeFiles(t, dir, c.files)
		if got := detectVerifyCommands(dir); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The detected commands are announced once, when a check first runs: a
// chat turn (nothing changed, nothing checked) says nothing about them.
func TestAutoVerifyCommandsAnnouncedOnce(t *testing.T) {
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "a"}, {content: "b"}, {content: "c"}}})
	eng.verifyGate = newAutoVerifyGate([]string{"go build ./..."}, t.TempDir())
	eng.verifyGate.ledgerPath = ""
	eng.verifyGate.runner = func(context.Context, string, string) (string, int, error) { return "", 0, nil }
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	run(t, eng, "hi")
	if strings.Contains(strings.Join(lines, "\n"), "完成校验命令") {
		t.Fatalf("a chat turn announced the check: %q", lines)
	}
	for _, msg := range []string{"edit one", "edit two"} {
		eng.fileMu.Lock()
		eng.turnFilesChanged = true
		eng.fileMu.Unlock()
		eng.verifyGate.onlyWhenFilesChanged = false // the turn start clears the flag set above
		run(t, eng, msg)
	}
	n := strings.Count(strings.Join(lines, "\n"), "完成校验命令：go build ./...")
	if n != 1 {
		t.Fatalf("announced %d times: %q", n, lines)
	}
}

// npm run build is added only for a package.json that declares a build script.
func TestDetectVerifyCommandsNpmNeedsBuildScript(t *testing.T) {
	for _, body := range []string{`{}`, `{"scripts":{}}`, `{"scripts":{"build":"  "}}`, `{"name":"x"}`} {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"package.json": body})
		for _, c := range detectVerifyCommands(dir) {
			if strings.Contains(c, "npm") {
				t.Errorf("%s: npm command %q without a build script", body, c)
			}
		}
	}
}
