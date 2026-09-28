package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/tool"
)

func writeCall(name, path string) api.ToolCall {
	return api.ToolCall{ID: "w1", Name: name, Input: map[string]any{"filePath": path, "content": "x"}}
}

// A rule on "write" did not apply to its alias "Write": in auto mode an
// in-project write went through although the rule said deny.
func TestRulesApplyToToolAliases(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	eng := newTestEngine(&mockProvider{}, tool.NewWriteTool())
	eng.perm.AddRule(permission.DDeny, permission.Rule{ToolPattern: "write"})

	target := filepath.Join(dir, "a.txt")
	out, _ := eng.executeTool(t.Context(), writeCall("Write", target))
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("the deny rule on write did not stop its alias Write: %s", out)
	}
	if !strings.HasPrefix(out, "Error:") {
		t.Fatalf("denied alias call did not report an error: %s", out)
	}
}

// Bypass skipped a policies.json deny rule that lives only in the policy
// engine (a glob tool pattern cannot become an e.perm rule); the manual says
// deny rules hold in every mode, bypass included.
func TestPolicyGlobDenyHoldsInBypass(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	eng := newTestEngine(&mockProvider{}, tool.NewWriteTool())
	eng.SetPermissionMode(permission.Bypass)
	// Written to policies.json and loaded the way a session start loads it.
	// COVE_CONFIG_DIR is shared by the package (main_test.go); a deny rule
	// left there would refuse writes in every later test.
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	path, err := PolicyFilePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := permission.NewFilePolicyStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save([]permission.PolicyRule{{ID: "no-writes", ToolPattern: "writ*", Action: permission.ActionDeny, Enabled: true, Priority: 10}}); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	eng.loadPersistedPolicies(cwd)

	target := filepath.Join(dir, "b.txt")
	out, _ := eng.executeTool(t.Context(), writeCall("write", target))
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("bypass wrote despite a policies.json deny: %s", out)
	}
}

// Guardrail warns after repeated failures and used to prepend its note, so
// exactly those failures lost their "Error:" prefix: shown as successes and
// resetting the consecutive-error breaker.
func TestGuardrailWarningKeepsTheErrorPrefix(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	eng := newTestEngine(&mockProvider{}, tool.NewReadTool())
	call := api.ToolCall{ID: "r1", Name: "read", Input: map[string]any{"filePath": filepath.Join(dir, "missing.txt")}}
	warned := false
	for i := 0; i < 4; i++ {
		out, _ := eng.executeTool(t.Context(), call)
		if !strings.HasPrefix(out, "Error:") {
			t.Fatalf("call %d: a failed read does not start with Error: %q", i+1, out)
		}
		warned = warned || strings.Contains(out, "[guardrail:")
	}
	if !warned {
		t.Fatal("guardrail never warned; the test no longer covers the warning path")
	}
}

// A call the safety checker blocks reports "BLOCKED …", which the old
// "Error:"-prefix test counted as a success (shown as OK, resetting the
// consecutive-failure breaker). The outcome flag says it failed.
func TestBlockedCallIsAFailure(t *testing.T) {
	isolatedHome(t)
	t.Chdir(t.TempDir())
	eng := newTestEngine(&mockProvider{}, &mockTool{name: "bash", result: "ran"})
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return true }
	out, failed := eng.executeTool(t.Context(), api.ToolCall{ID: "b1", Name: "bash", Input: map[string]any{"command": "rm -rf /"}})
	if !failed {
		t.Fatalf("a blocked call was not flagged as failed: %q", out)
	}
	if _, failed := eng.executeTool(t.Context(), api.ToolCall{ID: "b2", Name: "bash", Input: map[string]any{"command": "echo ok"}}); failed {
		t.Fatal("a successful call was flagged as failed")
	}
}

// refusingTool refuses every call in CheckPermissions and counts the calls
// that ran anyway.
type refusingTool struct{ ran int }

func (r *refusingTool) Def() tool.Def {
	return tool.Def{Name: "fetchy", InputSchema: []byte(`{"type":"object"}`), IsReadOnly: true}
}
func (r *refusingTool) Validate(tool.Input) string { return "" }
func (r *refusingTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.Denied("access to private/internal URLs is blocked")
}
func (r *refusingTool) Call(context.Context, tool.Input, tool.Context) (tool.Result, error) {
	r.ran++
	return tool.Result{Data: "fetched"}, nil
}

// A tool's own refusal is final. webfetch refuses private addresses in
// CheckPermissions, and an "always allow webfetch" rule, or bypass, used to
// override it: the refusal only served as the mode default.
func TestToolRefusalBeatsAllowRules(t *testing.T) {
	isolatedHome(t)
	t.Chdir(t.TempDir())
	rt := &refusingTool{}
	eng := newTestEngine(&mockProvider{}, rt)
	eng.perm.AddRule(permission.DAllow, permission.Rule{ToolPattern: "fetchy"})
	for _, mode := range []permission.Mode{permission.Default, permission.Auto, permission.Bypass} {
		eng.SetPermissionMode(mode)
		if out, failed := eng.executeTool(t.Context(), api.ToolCall{ID: "f", Name: "fetchy"}); !failed || rt.ran != 0 {
			t.Fatalf("%s: the tool ran despite refusing (ran=%d): %s", mode, rt.ran, out)
		}
	}
}
