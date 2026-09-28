package engine

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/tool"
)

// The per-turn injections (repo map excerpt, memories) are sized to the
// model's window: a 16K local model cannot spend 12KB on a repo map excerpt
// that a 200K model hardly notices.
func TestInjectionBudgetsScaleWithTheContextWindow(t *testing.T) {
	api.ClearModelContextWindows()
	t.Cleanup(api.ClearModelContextWindows)
	api.SetModelContextWindow("tiny-local", 16384)
	api.SetModelContextWindow("big-cloud", 200000)

	if got := repoMapExcerptBudget("big-cloud"); got != repoMapExcerptMaxBytes {
		t.Fatalf("big window repo map budget = %d, want the full %d", got, repoMapExcerptMaxBytes)
	}
	if got := repoMapExcerptBudget("tiny-local"); got > 2048 || got < 512 {
		t.Fatalf("16K window repo map budget = %d, want between 512 and 2048 bytes", got)
	}
	if got := turnMemoryBudget("big-cloud"); got != turnMemoryNoteMaxBytes {
		t.Fatalf("big window memory budget = %d, want %d", got, turnMemoryNoteMaxBytes)
	}
	if got := turnMemoryBudget("tiny-local"); got > 1536 || got < 512 {
		t.Fatalf("16K window memory budget = %d, want between 512 and 1536 bytes", got)
	}
	if got := toolResultBudgetTokens("tiny-local"); got > 2500 || got < 1000 {
		t.Fatalf("16K window tool result budget = %d tokens, want 1000..2500", got)
	}
	if got := toolResultBudgetTokens("big-cloud"); got < 10000 {
		t.Fatalf("200K window tool result budget = %d tokens, want >= 10000", got)
	}
}

// With a small window the request carries only the core tools: 25 tool
// definitions were ~3.9K tokens, a quarter of a 16K window, before the
// user said a word.
func TestSmallWindowSendsOnlyCoreToolDefinitions(t *testing.T) {
	api.ClearModelContextWindows()
	t.Cleanup(api.ClearModelContextWindows)
	api.SetModelContextWindow("tiny-local", 16384)

	tools := []string{"read", "write", "edit", "bash", "glob", "grep", "todowrite", "question", "agent", "task_list", "team_create", "worktree", "cron", "browser"}
	var mocks []tool.Tool
	for _, n := range tools {
		mocks = append(mocks, &mockTool{name: n, result: "ok"})
	}
	eng := newTestEngine(&mockProvider{}, mocks...)
	eng.config.Model = "tiny-local"

	names := map[string]bool{}
	for _, d := range eng.buildAPIToolDefs() {
		names[d.Name] = true
	}
	for _, core := range []string{"read", "write", "edit", "bash", "glob", "grep", "todowrite", "question"} {
		if !names[core] {
			t.Errorf("core tool %s missing from a small-window request", core)
		}
	}
	for _, extra := range []string{"agent", "task_list", "team_create", "worktree", "cron", "browser"} {
		if names[extra] {
			t.Errorf("non-core tool %s sent to a 16K model", extra)
		}
	}

	eng.config.Model = "roomy"
	api.SetModelContextWindow("roomy", 128000)
	all := eng.buildAPIToolDefs()
	if len(all) != len(tools) {
		t.Fatalf("big window sends %d tools, want all %d", len(all), len(tools))
	}
}

// A tool result larger than the window can afford is cut in the middle and
// says so; a small one is untouched.
func TestToolResultsAreCappedToTheWindow(t *testing.T) {
	api.ClearModelContextWindows()
	t.Cleanup(api.ClearModelContextWindows)
	api.SetModelContextWindow("tiny-local", 16384)

	big := strings.Repeat("line of tool output that goes on\n", 3000) // ~100KB
	got := capToolResult("tiny-local", "read", big)
	if len(got) >= len(big)/4 {
		t.Fatalf("capped result still %d bytes of %d", len(got), len(big))
	}
	if !strings.Contains(got, "truncated") || !strings.Contains(got, "16384") {
		t.Fatalf("capped result lacks the truncation note with the window size:\n%s", got[len(got)-400:])
	}
	if !strings.HasPrefix(got, "line of tool output") || !strings.HasSuffix(strings.TrimSpace(got), "]") {
		t.Fatalf("capped result should keep the head and end with the note")
	}
	small := "short output"
	if capToolResult("tiny-local", "bash", small) != small {
		t.Fatalf("small result was altered")
	}
}

// The automatic repo map excerpt describes the current repository; a task
// that names a directory elsewhere gets none of it.
func TestRepoMapExcerptSkippedWhenTheTaskTargetsAnotherDirectory(t *testing.T) {
	root := `D:\github\cove-main`
	cases := map[string]bool{
		`D:\github\agent 在该目录写一个netcore的agent框架的项目`:              true,
		`把 D:\github\cove-main\internal\engine\engine.go 里的函数拆开`: false,
		`/home/me/other-project 下建一个脚本`:                          true,
		`修复 internal/engine/turn.go 的报错`:                         false,
		`d:\GitHub\Cove-Main\docs 目录整理一下`:                        false,
	}
	for q, want := range cases {
		if got := queryTargetsOtherDirectory(q, root); got != want {
			t.Errorf("queryTargetsOtherDirectory(%q) = %v, want %v", q, got, want)
		}
	}
}

// The per-tool output cap scales down as well as up: a "read" limit of 6000
// tokens is over a third of a 16K window.
func TestToolOutputLimitShrinksForSmallWindows(t *testing.T) {
	api.ClearModelContextWindows()
	t.Cleanup(api.ClearModelContextWindows)
	api.SetModelContextWindow("tiny-local", 16384)
	if got, max := toolOutputLimit("read", "tiny-local"), toolResultBudgetTokens("tiny-local"); got > max {
		t.Fatalf("read limit %d exceeds the window's tool result budget %d", got, max)
	}
	if got := toolOutputLimit("read", "unknown-model"); got != 6000 {
		t.Fatalf("read limit for the default window = %d, want the tuned 6000", got)
	}
}

// Reducing the tool set is said once, so a person whose MCP or agent tools
// stopped appearing knows why.
func TestSmallWindowToolReductionIsAnnouncedOnce(t *testing.T) {
	api.ClearModelContextWindows()
	t.Cleanup(api.ClearModelContextWindows)
	api.SetModelContextWindow("tiny-local", 16384)
	eng := newTestEngine(&mockProvider{}, &mockTool{name: "read", result: "ok"}, &mockTool{name: "agent", result: "ok"})
	eng.config.Model = "tiny-local"
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	eng.buildAPIToolDefs()
	eng.buildAPIToolDefs()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "核心工具") && strings.Contains(l, "16384") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one announcement of the reduced tool set, got %d:\n%s", n, strings.Join(lines, "\n"))
	}
}
