package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/permission"
)

// writePolicies writes rules to the isolated home's policies.json.
func writePolicies(t *testing.T, home string, rules []permission.PolicyRule) {
	t.Helper()
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cove"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cove", "policies.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An ask rule in policies.json beats the read-only auto-allow.
func TestPolicyAskRuleBeatsReadOnlyAutoAllow(t *testing.T) {
	home := isolateHome(t)
	writePolicies(t, home, []permission.PolicyRule{
		{ID: "ask-git-status", ToolPattern: "bash", Action: permission.ActionAsk, Enabled: true, CommandPrefix: "git status"},
	})
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	runShell(t, eng, "bash", "git status")
	if p.count() != 1 {
		t.Fatalf("ask rule on read-only git status prompted %d times, want 1: %v", p.count(), p.asked)
	}
	runShell(t, eng, "bash", "git diff")
	if p.count() != 1 {
		t.Fatalf("ask rule leaked to git diff: prompted %d times", p.count())
	}
}

// A deny rule in policies.json still applies in bypass mode.
func TestPolicyDenyRuleBeatsBypass(t *testing.T) {
	home := isolateHome(t)
	writePolicies(t, home, []permission.PolicyRule{
		{ID: "deny-git-push", ToolPattern: "bash", Action: permission.ActionDeny, Enabled: true, CommandPrefix: "git push"},
	})
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Bypass, bash)
	eng.perm.SetBypassAvailable(true)
	out := runShell(t, eng, "bash", "git push origin main")
	if !strings.Contains(out, "denied") || bash.callCount() != 0 || p.count() != 0 {
		t.Fatalf("deny rule ignored in bypass mode: out=%q ran=%d prompted=%d", out, bash.callCount(), p.count())
	}
	if out := runShell(t, eng, "bash", "git commit -m x"); strings.HasPrefix(out, "Error") {
		t.Fatalf("bypass mode denied an unrelated command: %s", out)
	}
}

// An allow and a deny rule for the same prefix: deny wins.
func TestPolicyDenyBeatsAllowForSamePrefix(t *testing.T) {
	home := isolateHome(t)
	writePolicies(t, home, []permission.PolicyRule{
		{ID: "allow-bash-git push", ToolPattern: "bash", Action: permission.ActionAllow, Enabled: true, CommandPrefix: "git push"},
		{ID: "deny-git-push", ToolPattern: "bash", Action: permission.ActionDeny, Enabled: true, CommandPrefix: "git push"},
	})
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	out := runShell(t, eng, "bash", "git push")
	if !strings.Contains(out, "denied") || bash.callCount() != 0 || p.count() != 0 {
		t.Fatalf("allow beat deny for the same prefix: out=%q ran=%d prompted=%d", out, bash.callCount(), p.count())
	}
}

// /cd reloads persisted rules for the new project: rules scoped to the old
// project stop applying and the new project's rules start applying.
func TestSetWorkingDirReloadsPersistedRules(t *testing.T) {
	home := isolateHome(t)
	projA, projB := t.TempDir(), t.TempDir()
	rootA, rootB := permission.ProjectRoot(projA), permission.ProjectRoot(projB)
	writePolicies(t, home, []permission.PolicyRule{
		{ID: "allow-bash-git commit", ToolPattern: "bash", Action: permission.ActionAllow, Enabled: true, CommandPrefix: "git commit", Scope: rootA},
	})
	t.Chdir(projA)
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 0 {
		t.Fatalf("project A's rule not applied in A: %v", p.asked)
	}

	t.Chdir(projB)
	eng.SetWorkingDir(projB)
	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 1 {
		t.Fatalf("project A's rule applied after /cd to B: prompted %d, want 1", p.count())
	}

	if err := eng.PersistPermissionRule(permission.Rule{ToolPattern: "bash", CommandPrefix: "go build"}, rootB); err != nil {
		t.Fatal(err)
	}
	eng.SetWorkingDir(projB)
	runShell(t, eng, "bash", "go build ./...")
	if p.count() != 1 {
		t.Fatalf("project B's rule not applied in B after reload: prompted %d, want 1", p.count())
	}

	t.Chdir(projA)
	eng.SetWorkingDir(projA)
	runShell(t, eng, "bash", "go build ./...")
	if p.count() != 2 {
		t.Fatalf("project B's rule applied after /cd to A: prompted %d, want 2", p.count())
	}
	runShell(t, eng, "bash", "git commit -m y")
	if p.count() != 2 {
		t.Fatalf("project A's rule not reloaded after /cd back: prompted %d, want 2", p.count())
	}
}
