package main

import (
	"testing"

	"github.com/liuzhixin405/cove/internal/mcp"
)

func namesWith(o toolOptions) map[string]bool {
	names := map[string]bool{}
	for _, t := range registerToolsWith(mcp.NewPool(), o).All() {
		names[t.Def().Name] = true
	}
	return names
}

var experimentalToolNames = []string{
	"task", "task_list", "task_update", "task_stop", "task_get", "task_output",
	"team_create", "team_delete", "send_message", "brief", "sleep",
}

func TestExperimentalToolsOffByDefault(t *testing.T) {
	names := namesWith(toolOptions{goos: "linux", interactive: true})
	for _, n := range experimentalToolNames {
		if names[n] {
			t.Errorf("%s registered without experimental_tools", n)
		}
	}
	for _, core := range []string{"bash", "read", "edit", "todowrite", "agent", "execute_plan", "websearch"} {
		if !names[core] {
			t.Errorf("core tool %s missing", core)
		}
	}
	on := namesWith(toolOptions{goos: "linux", interactive: true, experimental: true})
	for _, n := range experimentalToolNames {
		if !on[n] {
			t.Errorf("%s missing with experimental_tools", n)
		}
	}
}

func TestQuestionOnlyInteractive(t *testing.T) {
	if namesWith(toolOptions{goos: "linux", interactive: false})["question"] {
		t.Error("question registered in non-interactive mode")
	}
	if !namesWith(toolOptions{goos: "linux", interactive: true})["question"] {
		t.Error("question missing in interactive mode")
	}
}

func TestBrowserOnlyWithChrome(t *testing.T) {
	if namesWith(toolOptions{goos: "linux", interactive: true, chrome: false})["browser"] {
		t.Error("browser registered without headless Chrome compiled in")
	}
	if !namesWith(toolOptions{goos: "linux", interactive: true, chrome: true})["browser"] {
		t.Error("browser missing with Chrome")
	}
}

// The mode comes from main's branch, not from re-reading os.Args.
func TestToolsInteractiveForMode(t *testing.T) {
	oldTUI, oldNoTUI := tuiMode, noTUI
	t.Cleanup(func() { tuiMode, noTUI = oldTUI, oldNoTUI })
	t.Setenv("COVE_TUI", "")
	tuiMode, noTUI = true, false
	if toolsInteractiveFor(true) {
		t.Error("-p must not be interactive")
	}
	if !toolsInteractiveFor(false) {
		t.Error("forced TUI should be interactive")
	}
	tuiMode, noTUI = false, true
	if toolsInteractiveFor(false) {
		t.Error("--no-tui (headless) must not be interactive")
	}
}

func TestRegisterAllToolsHonoursInteractive(t *testing.T) {
	has := func(interactive bool) bool {
		_, ok := registerAllTools(mcp.NewPool(), nil, interactive).Find("question")
		return ok
	}
	if has(false) || !has(true) {
		t.Fatalf("question registered: non-interactive=%v interactive=%v", has(false), has(true))
	}
}
