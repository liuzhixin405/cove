package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/permission"
)

// A "[p] 永久允许" answer is written to policies.json and applies to
// the next engine started in the same project.
func TestPersistPermissionRuleSurvivesRestart(t *testing.T) {
	home := isolateHome(t)
	cwd, _ := os.Getwd()
	root := permission.ProjectRoot(cwd)

	bash := &permShellTool{name: "bash"}
	eng, _ := newPermEngine(t, permission.Default, bash)
	if err := eng.PersistPermissionRule(permission.Rule{ToolPattern: "bash", CommandPrefix: "git commit"}, root); err != nil {
		t.Fatalf("PersistPermissionRule: %v", err)
	}
	// Persisting the same rule twice keeps one entry.
	if err := eng.PersistPermissionRule(permission.Rule{ToolPattern: "bash", CommandPrefix: "git commit"}, root); err != nil {
		t.Fatalf("PersistPermissionRule again: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".cove", "policies.json"))
	if err != nil {
		t.Fatalf("policies.json not written: %v", err)
	}
	var rules []permission.PolicyRule
	if err := json.Unmarshal(data, &rules); err != nil {
		t.Fatalf("policies.json is not a rule array: %v\n%s", err, data)
	}
	if len(rules) != 1 {
		t.Fatalf("policies.json holds %d rules, want 1:\n%s", len(rules), data)
	}
	r := rules[0]
	if r.ID != "allow-bash-git commit" || r.ToolPattern != "bash" || r.Action != permission.ActionAllow ||
		!r.Enabled || r.CommandPrefix != "git commit" || !permission.SameProject(r.Scope, root) {
		t.Fatalf("persisted rule = %+v", r)
	}

	// Restart: a new engine in the same project runs git commit unasked,
	// but nothing wider than the prefix.
	bash2 := &permShellTool{name: "bash"}
	eng2, p2 := newPermEngine(t, permission.Default, bash2)
	// The quoted-operator case depends on the shell; pin Git Bash semantics
	// so the test does not depend on which shell this machine resolves.
	eng2.perm.SetShellKind(permission.ShellPOSIX)
	runShell(t, eng2, "bash", "git commit -m x")
	runShell(t, eng2, "bash", `git commit -m "fix(api): x; y"`)
	if p2.count() != 0 || bash2.callCount() != 2 {
		t.Fatalf("after restart git commit prompted %d times, ran %d: %v", p2.count(), bash2.callCount(), p2.asked)
	}
	runShell(t, eng2, "bash", "git push")
	runShell(t, eng2, "bash", "git commit -m x && rm -rf build")
	if p2.count() != 2 {
		t.Fatalf("persisted prefix rule leaked to other commands: prompted %d, want 2", p2.count())
	}
}

// A rule persisted for another project is ignored; a rule with an empty
// scope applies everywhere.
func TestPersistedRulesAreScopedToTheirProject(t *testing.T) {
	home := isolateHome(t)
	other := t.TempDir()
	rules := []permission.PolicyRule{
		{ID: "allow-bash-git commit", ToolPattern: "bash", Action: permission.ActionAllow, Enabled: true, CommandPrefix: "git commit", Scope: other},
		{ID: "allow-bash-go test", ToolPattern: "bash", Action: permission.ActionAllow, Enabled: true, CommandPrefix: "go test", Scope: ""},
	}
	data, _ := json.MarshalIndent(rules, "", "  ")
	if err := os.MkdirAll(filepath.Join(home, ".cove"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cove", "policies.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	runShell(t, eng, "bash", "go test ./...")
	if p.count() != 0 {
		t.Fatalf("global go test rule not applied: %v", p.asked)
	}
	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 1 {
		t.Fatalf("another project's git commit rule applied here: prompted %d, want 1", p.count())
	}

	// Persisting in this project keeps the other project's rule in the file.
	cwd, _ := os.Getwd()
	if err := eng.PersistPermissionRule(permission.Rule{ToolPattern: "bash", CommandPrefix: "git commit"}, permission.ProjectRoot(cwd)); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(home, ".cove", "policies.json"))
	var got []permission.PolicyRule
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("policies.json has %d rules, want 3 (other project's kept):\n%s", len(got), data)
	}
	if !strings.Contains(string(data), `"command_prefix": "git commit"`) {
		t.Errorf("unexpected file format:\n%s", data)
	}
}

// A deny rule in policies.json beats the read-only auto-allow.
func TestPolicyDenyRuleBeatsReadOnlyAutoAllow(t *testing.T) {
	home := isolateHome(t)
	data, _ := json.Marshal([]permission.PolicyRule{
		{ID: "deny-git-log", ToolPattern: "bash", Action: permission.ActionDeny, Enabled: true, CommandPrefix: "git log"},
	})
	if err := os.MkdirAll(filepath.Join(home, ".cove"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cove", "policies.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	bash := &permShellTool{name: "bash"}
	eng, _ := newPermEngine(t, permission.Default, bash)
	if out := runShell(t, eng, "bash", "git status && git log"); !strings.Contains(out, "denied") {
		t.Fatalf("policy deny rule ignored for a read-only line: %s", out)
	}
	if out := runShell(t, eng, "bash", "git status"); strings.HasPrefix(out, "Error") {
		t.Fatalf("unrelated read-only command denied: %s", out)
	}
}

// A policies.json that cannot be parsed is reported, not silently ignored,
// and is left untouched.
func TestUnreadablePoliciesFileIsReported(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".cove", "policies.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, _ := newPermEngine(t, permission.Default, &permShellTool{name: "bash"})
	if err := eng.PolicyLoadError(); err == nil || !strings.Contains(err.Error(), "policies.json") {
		t.Fatalf("PolicyLoadError() = %v, want an error naming policies.json", err)
	}
	if err := eng.PersistPermissionRule(permission.Rule{ToolPattern: "bash", CommandPrefix: "go test"}, ""); err == nil {
		t.Error("persisting over an unreadable policies.json must fail, not overwrite it")
	}
	if data, _ := os.ReadFile(path); string(data) != "[{broken" {
		t.Errorf("policies.json was rewritten: %q", data)
	}
}

// A policies.json that becomes corrupt after startup must not drop the
// rules already loaded (fail-open): reloading (via /cd -> SetWorkingDir)
// keeps the previous rules in effect and reports the error, instead of
// silently letting a deny rule stop applying.
func TestCorruptPolicyReloadKeepsPreviousDenyRule(t *testing.T) {
	home := isolateHome(t)
	policyPath := filepath.Join(home, ".cove", "policies.json")
	data, _ := json.Marshal([]permission.PolicyRule{
		{ID: "deny-git-log", ToolPattern: "bash", Action: permission.ActionDeny, Enabled: true, CommandPrefix: "git log"},
	})
	if err := os.MkdirAll(filepath.Dir(policyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	bash := &permShellTool{name: "bash"}
	eng, _ := newPermEngine(t, permission.Default, bash)
	if err := eng.PolicyLoadError(); err != nil {
		t.Fatalf("PolicyLoadError() = %v, want nil after a valid load", err)
	}
	if out := runShell(t, eng, "bash", "git log"); !strings.Contains(out, "denied") {
		t.Fatalf("deny rule not applied before corruption: %s", out)
	}

	// Corrupt the file, then move the engine to another directory: this
	// re-triggers loadPersistedPolicies via SetWorkingDir.
	if err := os.WriteFile(policyPath, []byte("[{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng.SetWorkingDir(t.TempDir())

	if err := eng.PolicyLoadError(); err == nil || !strings.Contains(err.Error(), "policies.json") {
		t.Fatalf("PolicyLoadError() = %v, want an error naming policies.json", err)
	}
	if out := runShell(t, eng, "bash", "git log"); !strings.Contains(out, "denied") {
		t.Fatalf("deny rule dropped after corrupt reload (fail-open): %s", out)
	}
}

// Several prefixes from one "[p]" answer are written in a single save.
func TestPersistPermissionRulesWritesAllAtOnce(t *testing.T) {
	home := isolateHome(t)
	eng, _ := newPermEngine(t, permission.Default, &permShellTool{name: "bash"})
	scope := eng.PermissionScope()
	cwd, _ := os.Getwd()
	if !permission.SameProject(scope, permission.ProjectRoot(cwd)) {
		t.Fatalf("PermissionScope() = %q, want the project root of %q", scope, cwd)
	}
	err := eng.PersistPermissionRules([]permission.Rule{
		{ToolPattern: "bash", CommandPrefix: "go test"},
		{ToolPattern: "bash", CommandPrefix: "tee"},
	}, scope)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".cove", "policies.json"))
	var got []permission.PolicyRule
	if err := json.Unmarshal(data, &got); err != nil || len(got) != 2 {
		t.Fatalf("policies.json = %s (err %v), want 2 rules", data, err)
	}
}

// A "[p]" rule is installed by PersistPermissionRules itself as a disk rule,
// so /cd to another project drops it like every other rule loaded from
// policies.json: the same command asks again there.
func TestPersistedRuleIsDroppedOnCdToAnotherProject(t *testing.T) {
	isolateHome(t)
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	eng.perm.SetShellKind(permission.ShellPOSIX)
	if err := eng.PersistPermissionRules([]permission.Rule{{ToolPattern: "bash", CommandPrefix: "git commit"}}, eng.PermissionScope()); err != nil {
		t.Fatal(err)
	}
	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 0 {
		t.Fatalf("persisted rule not applied in this session: prompted %d", p.count())
	}
	eng.SetWorkingDir(t.TempDir())
	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 1 {
		t.Fatalf("after /cd to another project git commit prompted %d times, want 1", p.count())
	}
}

// Persisting the same rule twice does not stack copies that /cd would only
// half remove.
func TestPersistSameRuleTwiceRegistersOnce(t *testing.T) {
	isolateHome(t)
	eng, _ := newPermEngine(t, permission.Default, &permShellTool{name: "bash"})
	r := permission.Rule{ToolPattern: "bash", CommandPrefix: "go test"}
	for i := 0; i < 2; i++ {
		if err := eng.PersistPermissionRules([]permission.Rule{r}, eng.PermissionScope()); err != nil {
			t.Fatal(err)
		}
	}
	if len(eng.diskRules) != 1 {
		t.Fatalf("diskRules = %+v, want one entry", eng.diskRules)
	}
	eng.SetWorkingDir(t.TempDir())
	if d, _ := eng.perm.Check("bash", map[string]any{"command": "go test ./..."}, permission.DAsk); d == permission.DAllow {
		t.Fatal("a copy of the persisted rule survived /cd")
	}
}

// An ask rule added in the session beats an allow rule from policies.json:
// the policy engine must not turn the manager's ask back into an allow.
func TestSessionAskRuleBeatsPolicyFileAllow(t *testing.T) {
	home := isolateHome(t)
	writePolicies(t, home, []permission.PolicyRule{
		{ID: "allow-bash-git commit", ToolPattern: "bash", Action: permission.ActionAllow, Enabled: true, CommandPrefix: "git commit"},
	})
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	eng.AddPermissionRule(permission.DAsk, permission.Rule{ToolPattern: "bash", CommandPrefix: "git commit"})
	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 1 {
		t.Fatalf("session ask + policies.json allow prompted %d times, want 1", p.count())
	}
}

// policies.json holding both an allow and an ask rule for the same command
// (allow written first) asks.
func TestPolicyFileAskBeatsAllowWrittenFirst(t *testing.T) {
	home := isolateHome(t)
	writePolicies(t, home, []permission.PolicyRule{
		{ID: "allow-bash-git commit", ToolPattern: "bash", Action: permission.ActionAllow, Enabled: true, CommandPrefix: "git commit"},
		{ID: "ask-bash-git commit", ToolPattern: "bash", Action: permission.ActionAsk, Enabled: true, CommandPrefix: "git commit"},
	})
	bash := &permShellTool{name: "bash"}
	eng, p := newPermEngine(t, permission.Default, bash)
	runShell(t, eng, "bash", "git commit -m x")
	if p.count() != 1 {
		t.Fatalf("policies.json allow+ask prompted %d times, want 1", p.count())
	}
}
