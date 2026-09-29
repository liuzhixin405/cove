package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/notes"
)

func writeTreeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTestCommandsForGoPackages(t *testing.T) {
	root := t.TempDir()
	writeTreeFiles(t, root, map[string]string{
		"go.mod":          "module x\n",
		"a/a.go":          "package a\n",
		"a/a_test.go":     "package a\n",
		"b/c/c.go":        "package c\n",
		"testdata/fix.go": "package fix\n",
		"main.go":         "package main\n",
		"docs/readme.md":  "x",
		"../outside/o.go": "package o\n",
	})
	files := []string{
		filepath.Join(root, "a", "a.go"), filepath.Join(root, "a", "a_test.go"),
		filepath.Join(root, "b", "c", "c.go"), filepath.Join(root, "testdata", "fix.go"),
		filepath.Join(root, "main.go"), filepath.Join(root, "docs", "readme.md"),
		filepath.Join(root, "..", "outside", "o.go"),
	}
	got := testCommandsFor(root, files)
	want := []string{"go vet . ./a ./b/c", "go test . ./a ./b/c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if got := testCommandsFor(root, []string{filepath.Join(root, "docs", "readme.md")}); len(got) != 0 {
		t.Fatalf("a docs change ran tests: %q", got)
	}
}

func TestTestCommandsForDotnetTestProjects(t *testing.T) {
	root := t.TempDir()
	writeTreeFiles(t, root, map[string]string{
		"src/Shop/Shop.csproj":                 "<Project/>",
		"src/Shop/Orders/OrderService.cs":      "class OrderService {}",
		"src/Other/Other.csproj":               "<Project/>",
		"tests/Shop.Tests/Shop.Tests.csproj":   `<Project><ItemGroup><ProjectReference Include="..\..\src\Shop\Shop.csproj" /></ItemGroup></Project>`,
		"tests/Other.Tests/Other.Tests.csproj": `<Project><ItemGroup><ProjectReference Include="..\..\src\Other\Other.csproj" /></ItemGroup></Project>`,
	})
	got := testCommandsFor(root, []string{filepath.Join(root, "src", "Shop", "Orders", "OrderService.cs")})
	if len(got) != 1 || !strings.Contains(got[0], "dotnet test") || !strings.Contains(got[0], "Shop.Tests.csproj") {
		t.Fatalf("commands = %q", got)
	}
}

func TestPythonTestsFor(t *testing.T) {
	root := t.TempDir()
	writeTreeFiles(t, root, map[string]string{
		"pkg/billing.py":        "",
		"tests/test_billing.py": "",
		"pkg/test_util.py":      "",
		"pkg/other.py":          "",
	})
	got := pythonTestsFor(root, []string{
		filepath.Join(root, "pkg", "billing.py"), filepath.Join(root, "pkg", "test_util.py"), filepath.Join(root, "pkg", "other.py"),
	})
	if strings.Join(got, " ") != "pkg/test_util.py tests/test_billing.py" {
		t.Fatalf("targets = %q", got)
	}
}

// The gate runs the fixed commands, then the ones derived for the turn.
func TestVerifyGateRunsDynamicCommands(t *testing.T) {
	var ran []string
	g := NewVerifyGate([]string{"go build ./..."}, t.TempDir())
	g.ledgerPath = ""
	g.dynamic = func() []string { return []string{"go test ./a"} }
	g.runner = func(_ context.Context, cmd, _ string) (string, int, error) {
		ran = append(ran, cmd)
		if cmd == "go test ./a" {
			return "--- FAIL: TestX", 1, nil
		}
		return "", 0, nil
	}
	results, passed := g.Run(context.Background(), nil)
	if passed || strings.Join(ran, "|") != "go build ./...|go test ./a" {
		t.Fatalf("ran %q, passed %v", ran, passed)
	}
	if !strings.Contains(Summary(results), "FAIL go test ./a") {
		t.Fatalf("summary:\n%s", Summary(results))
	}
	if defaultVerifyTimeout("go test ./a") != verifyTimeoutSlow || defaultVerifyTimeout("go build ./...") != verifyTimeoutDefault {
		t.Fatal("go test should get the slow timeout, go build the default")
	}
}

// An unfinished plan goes to the model only when the user asks to continue;
// otherwise the user is told about it, once. A finished one is cleared.
func TestPreviousPlanNote(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	eng.sessionNotes = notes.New(t.TempDir())
	eng.runtime.SetTodos(sampleTodos())
	eng.persistPlan()

	eng.resetConversationState()
	if note := eng.previousPlanNote("/context"); note != "" {
		t.Fatalf("an unrelated message got the old plan: %q", note)
	}
	eng.previousPlanNote("看看代码")
	if n := strings.Count(strings.Join(lines, ""), "未完成的计划"); n != 1 {
		t.Fatalf("user told %d times: %q", n, lines)
	}
	note := eng.previousPlanNote("继续上次的计划")
	if !strings.Contains(note, "<previous_plan>") || !strings.Contains(note, "fix the tokenizer") {
		t.Fatalf("note = %q", note)
	}

	eng.runtime.SetTodos([]any{map[string]any{"content": "fix the tokenizer", "status": "completed", "priority": "high"}})
	eng.persistPlan()
	eng.resetConversationState()
	eng.runtime.ClearTodos()
	if note := eng.previousPlanNote("继续"); note != "" {
		t.Fatalf("a finished plan was offered: %q", note)
	}
}

type fakeReviewer struct {
	task   string
	output string
}

func (f *fakeReviewer) Run(_ context.Context, name, task string) (*api.AgentRunResult, error) {
	f.task = name + ":" + task
	return &api.AgentRunResult{Output: f.output, Success: true}, nil
}
func (f *fakeReviewer) Register(string, string, string) {}

func TestSelfReview(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	writeTreeFiles(t, dir, map[string]string{"calc.go": "package calc\n\nfunc Div(a, b int) int { return a / b }\n"})
	eng := newTestEngine(&mockProvider{})
	rev := &fakeReviewer{output: "calc.go:3 — division by zero when b is 0 — Div is called with user input"}
	eng.runtime.AgentRunner = rev
	eng.config.DoneSelfReview = "on"
	eng.turnChangedFiles = map[string]bool{filepath.Join(dir, "calc.go"): true}
	eng.projCtx = nil
	// projectCwd is "" without a project context; the diff uses the paths.
	got := eng.selfReview(context.Background(), "add Div")
	if !strings.Contains(got, "division by zero") || !strings.HasPrefix(rev.task, "review:") || !strings.Contains(rev.task, "func Div") {
		t.Fatalf("findings %q, task %q", got, rev.task)
	}
	if eng.selfReview(context.Background(), "add Div") != "" {
		t.Fatal("reviewed twice in one turn")
	}

	eng.selfReviewed = false
	rev.output = "NO_ISSUES"
	if got := eng.selfReview(context.Background(), "add Div"); got != "" {
		t.Fatalf("clean review returned %q", got)
	}

	eng.selfReviewed = false
	eng.config.DoneSelfReview = "auto" // 3 lines < selfReviewMinLines
	rev.task = ""
	if eng.selfReview(context.Background(), "add Div"); rev.task != "" {
		t.Fatal("auto reviewed a small change")
	}
}
