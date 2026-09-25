package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/permission"
)

// policies.json lives in the config directory, like config.json: with
// COVE_CONFIG_DIR set, both are read and written there, not in ~/.cove.
func TestPoliciesFollowCoveConfigDir(t *testing.T) {
	home := isolateHome(t)
	dir := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", dir)
	data, _ := json.Marshal([]permission.PolicyRule{
		{ID: "deny-git-log", ToolPattern: "bash", Action: permission.ActionDeny, Enabled: true, CommandPrefix: "git log"},
	})
	if err := os.WriteFile(filepath.Join(dir, "policies.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	eng, _ := newPermEngine(t, permission.Default, &permShellTool{name: "bash"})
	if out := runShell(t, eng, "bash", "git log"); !strings.Contains(out, "denied") {
		t.Fatalf("deny rule in COVE_CONFIG_DIR/policies.json ignored: %s", out)
	}
	if err := eng.PersistPermissionRule(permission.Rule{ToolPattern: "bash", CommandPrefix: "go test"}, ""); err != nil {
		t.Fatal(err)
	}
	var rules []permission.PolicyRule
	data, _ = os.ReadFile(filepath.Join(dir, "policies.json"))
	if err := json.Unmarshal(data, &rules); err != nil || len(rules) != 2 {
		t.Fatalf("persisted rules in COVE_CONFIG_DIR = %d (%v):\n%s", len(rules), err, data)
	}
	if _, err := os.Stat(filepath.Join(home, ".cove", "policies.json")); err == nil {
		t.Fatal("policies.json was also written under ~/.cove")
	}
}

// Engine tests never see the developer's real policies: TestMain gives the
// package its own empty config directory.
func TestEngineTestsUseAnIsolatedConfigDir(t *testing.T) {
	dir := os.Getenv("COVE_CONFIG_DIR")
	if dir == "" {
		t.Fatal("COVE_CONFIG_DIR is not set for engine tests")
	}
	if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(os.TempDir())) {
		t.Fatalf("engine tests use config dir %s, not a temporary one", dir)
	}
	path, err := policyFilePath()
	if err != nil || path != filepath.Join(dir, "policies.json") {
		t.Fatalf("policyFilePath() = %q, %v; want it under %s", path, err, dir)
	}
}
