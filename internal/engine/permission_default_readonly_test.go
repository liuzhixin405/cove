package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/tool"
)

// permShellTool stands in for the bash/powershell tools: it applies the same
// mode-based CheckPermissions as the real ones and records the commands it
// was asked to run, without ever starting a process.
type permShellTool struct {
	name  string
	mu    sync.Mutex
	calls []string
}

func (f *permShellTool) Def() tool.Def {
	return tool.Def{
		Name:              f.name,
		Description:       "fake shell",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		IsConcurrencySafe: true,
		UserFacingName:    f.name,
	}
}

func (f *permShellTool) Validate(tool.Input) string { return "" }

func (f *permShellTool) CheckPermissions(input tool.Input, tctx tool.Context) tool.PermissionDecision {
	switch tctx.PermissionMode {
	case "bypass", "auto":
		return tool.PermissionDecision{Decision: tool.Allow, Reason: "mode: " + tctx.PermissionMode}
	case "plan":
		return tool.PermissionDecision{Decision: tool.Deny, Reason: "plan mode"}
	}
	return tool.PermissionDecision{Decision: tool.Ask, Reason: f.name + " requires approval"}
}

func (f *permShellTool) Call(_ context.Context, input tool.Input, _ tool.Context) (tool.Result, error) {
	cmd, _ := input["command"].(string)
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	return tool.Result{Data: "ok"}, nil
}

func (f *permShellTool) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// countingPrompt approves every call and counts how often it was asked.
type countingPrompt struct {
	mu    sync.Mutex
	asked []string
}

func (p *countingPrompt) prompt(toolName string, input map[string]any, _ string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd, _ := input["command"].(string)
	p.asked = append(p.asked, toolName+": "+cmd)
	return true
}

func (p *countingPrompt) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.asked)
}

// isolateHome points HOME (and USERPROFILE on Windows) at a temp dir so the
// engine neither reads the user's policies.json nor writes session
// files there, and runs the test in a fresh working directory. The config
// directory, where policies.json lives, is that home's .cove.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	t.Chdir(t.TempDir())
	return home
}

func newPermEngine(t *testing.T, mode permission.Mode, tools ...tool.Tool) (*Engine, *countingPrompt) {
	t.Helper()
	eng := newTestEngine(&mockProvider{}, tools...)
	eng.config.PermissionMode = string(mode)
	eng.perm.SetMode(mode)
	eng.classifier = permission.NewClassifier()
	p := &countingPrompt{}
	eng.PermissionPrompt = p.prompt
	return eng, p
}

func runShell(t *testing.T, eng *Engine, toolName, cmd string) string {
	t.Helper()
	return eng.executeTool(context.Background(), api.ToolCall{ID: "t-" + cmd, Name: toolName, Input: map[string]any{"command": cmd}})
}

func TestDefaultModeRunsReadOnlyShellCommandsWithoutAsking(t *testing.T) {
	isolateHome(t)
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)

	for _, cmd := range []string{"git status", "git diff --stat", "git log --oneline -5", "git status && git diff", "ls -la"} {
		if out := runShell(t, eng, "bash", cmd); strings.HasPrefix(out, "Error") {
			t.Fatalf("%q: %s", cmd, out)
		}
	}
	if p.count() != 0 {
		t.Fatalf("read-only commands prompted %d times: %v", p.count(), p.asked)
	}

	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 1 {
		t.Fatalf("git commit prompted %d times, want 1: %v", p.count(), p.asked)
	}
	if bash.callCount() != 6 {
		t.Fatalf("bash ran %d commands, want 6", bash.callCount())
	}
}

func TestDefaultModeReadOnlyAppliesToPowerShellToo(t *testing.T) {
	isolateHome(t)
	ps := &permShellTool{name: "powershell"}
	eng, p := newPermEngine(t, permission.Default, ps)

	runShell(t, eng, "powershell", "Get-ChildItem -Recurse")
	runShell(t, eng, "powershell", "git status")
	if p.count() != 0 {
		t.Fatalf("read-only powershell commands prompted: %v", p.asked)
	}
	runShell(t, eng, "powershell", "Remove-Item x")
	if p.count() != 1 {
		t.Fatalf("Remove-Item prompted %d times, want 1", p.count())
	}
	if out := runShell(t, eng, "powershell", `Remove-Item -Recurse -Force C:\`); !strings.Contains(strings.ToLower(out), "blocked") {
		t.Fatalf("catastrophic powershell command not blocked: %s", out)
	}
}

// A deny or ask rule the user configured still wins over the read-only
// shortcut.
func TestDefaultModeReadOnlyStillHonoursDenyAndAskRules(t *testing.T) {
	isolateHome(t)
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	eng.AddPermissionRule(permission.DDeny, permission.Rule{ToolPattern: "bash", CommandPrefix: "git log"})
	eng.AddPermissionRule(permission.DAsk, permission.Rule{ToolPattern: "bash", CommandPrefix: "git diff"})

	if out := runShell(t, eng, "bash", "git log"); !strings.Contains(out, "denied") {
		t.Fatalf("deny rule ignored: %s", out)
	}
	runShell(t, eng, "bash", "git diff")
	if p.count() != 1 {
		t.Fatalf("ask rule ignored: prompted %d times", p.count())
	}
}

// Mode tiers: auto additionally runs build/test commands unasked, but git
// writes, installs and unknown commands still ask. Plan mode never runs bash.
func TestAutoModeTiers(t *testing.T) {
	isolateHome(t)
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Auto, bash)

	for _, cmd := range []string{"git status", "go test ./...", "go build ./... && go vet ./..."} {
		runShell(t, eng, "bash", cmd)
	}
	if p.count() != 0 {
		t.Fatalf("auto mode prompted for safe/build commands: %v", p.asked)
	}
	for _, cmd := range []string{"git commit -m x", "npm install x", "rm -rf build", "go run ."} {
		runShell(t, eng, "bash", cmd)
	}
	if p.count() != 4 {
		t.Fatalf("auto mode prompted %d times for git/install/unknown, want 4: %v", p.count(), p.asked)
	}

	eng.SetPermissionMode(permission.Plan)
	before := bash.callCount()
	runShell(t, eng, "bash", "git status")
	if bash.callCount() != before {
		t.Fatal("plan mode ran bash")
	}
}

func TestBypassModeAllowsEverything(t *testing.T) {
	isolateHome(t)
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Bypass, bash)
	runShell(t, eng, "bash", "git commit -m x")
	runShell(t, eng, "bash", "rm -rf build")
	if p.count() != 0 || bash.callCount() != 2 {
		t.Fatalf("bypass: prompted %d, ran %d", p.count(), bash.callCount())
	}
}

// In auto mode a write/edit inside the project runs unasked; outside it asks.
func TestAutoModeWriteInsideProjectOnly(t *testing.T) {
	isolateHome(t)
	w := &mockTool{name: "write", readOnly: false, result: "ok"}
	ed := &mockTool{name: "edit", readOnly: false, result: "ok"}
	eng, p := newPermEngine(t, permission.Auto, w, ed)

	eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "write", Input: map[string]any{"filePath": "a.go"}})
	eng.executeTool(context.Background(), api.ToolCall{ID: "2", Name: "edit", Input: map[string]any{"filePath": "sub/b.go"}})
	if p.count() != 0 {
		t.Fatalf("auto mode prompted for in-project write/edit: %v", p.asked)
	}
	outside := t.TempDir()
	eng.executeTool(context.Background(), api.ToolCall{ID: "3", Name: "write", Input: map[string]any{"filePath": outside + "/x.go"}})
	eng.executeTool(context.Background(), api.ToolCall{ID: "4", Name: "edit", Input: map[string]any{"filePath": "../escape.go"}})
	if p.count() != 2 {
		t.Fatalf("auto mode prompted %d times for out-of-project writes, want 2", p.count())
	}

	// Default mode asks even inside the project.
	eng.SetPermissionMode(permission.Default)
	eng.executeTool(context.Background(), api.ToolCall{ID: "5", Name: "write", Input: map[string]any{"filePath": "a.go"}})
	if p.count() != 3 {
		t.Fatalf("default mode write prompted %d times total, want 3", p.count())
	}
}
