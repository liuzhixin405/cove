package permission

import (
	"path/filepath"
	"testing"
)

func TestMatchRule_ArgPatternNilInputNoBypass(t *testing.T) {
	r := Rule{ToolPattern: "bash", Decision: DAllow, ArgPattern: "rm -rf"}
	// nil input must NOT match an arg-restricted rule (previously it fell through
	// to true, auto-allowing any bash call with a nil Input map).
	if matchRule(r, "bash", nil) {
		t.Fatal("arg-restricted rule must not match nil input")
	}
	if matchRule(r, "bash", map[string]any{"command": "ls"}) {
		t.Fatal("should not match when arg pattern absent")
	}
	if !matchRule(r, "bash", map[string]any{"command": "rm -rf /tmp/x"}) {
		t.Fatal("should match when arg pattern present")
	}
}

func TestPolicyEngine_EvaluateAndGlob(t *testing.T) {
	pe := NewPolicyEngine()
	_ = pe.AddRule(PolicyRule{ID: "a", ToolPattern: "read", Action: ActionAllow, Enabled: true, Priority: 10})
	_ = pe.AddRule(PolicyRule{ID: "b", ToolPattern: "mcp_*", Action: ActionDeny, Enabled: true, Priority: 5})

	if got := pe.Evaluate("read", nil, "default"); got != ActionAllow {
		t.Fatalf("read: want allow, got %v", got)
	}
	if got := pe.Evaluate("mcp_github_search", nil, "default"); got != ActionDeny {
		t.Fatalf("glob mcp_*: want deny, got %v", got)
	}
	if got := pe.Evaluate("write", nil, "default"); got != ActionAsk {
		t.Fatalf("unmatched in default mode: want ask, got %v", got)
	}
	if got := pe.Evaluate("write", nil, "auto"); got != ActionAllow {
		t.Fatalf("unmatched in auto mode: want allow, got %v", got)
	}
}

func TestPolicyEngine_HigherPriorityWins(t *testing.T) {
	pe := NewPolicyEngine()
	_ = pe.AddRule(PolicyRule{ID: "low", ToolPattern: "bash", Action: ActionAllow, Enabled: true, Priority: 1})
	_ = pe.AddRule(PolicyRule{ID: "high", ToolPattern: "bash", Action: ActionDeny, Enabled: true, Priority: 100})
	if got := pe.Evaluate("bash", nil, "default"); got != ActionDeny {
		t.Fatalf("higher-priority rule should win: want deny, got %v", got)
	}
}

// The "始终允许" flow must survive a restart: a rule added to an engine with a
// file store should be reloaded by a fresh engine pointed at the same file.
func TestPolicyEngine_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.json")
	store, err := NewFilePolicyStorage(path)
	if err != nil {
		t.Fatal(err)
	}

	pe := NewPolicyEngine()
	pe.SetStorage(store)
	if err := pe.AddRule(PolicyRule{ID: "user-allow-write", ToolPattern: "write", Action: ActionAllow, Enabled: true, Priority: 100}); err != nil {
		t.Fatal(err)
	}

	// Simulate restart: new engine, new store, same file.
	store2, _ := NewFilePolicyStorage(path)
	rules, err := store2.Load()
	if err != nil {
		t.Fatal(err)
	}
	pe2 := NewPolicyEngine()
	pe2.LoadRules(rules)
	if got := pe2.Evaluate("write", nil, "default"); got != ActionAllow {
		t.Fatalf("persisted rule lost across restart: want allow, got %v", got)
	}

	// Removal must persist too.
	pe.SetStorage(store) // ensure storage attached
	if err := pe.RemoveRule("user-allow-write"); err != nil {
		t.Fatal(err)
	}
	store3, _ := NewFilePolicyStorage(path)
	rules3, _ := store3.Load()
	if len(rules3) != 0 {
		t.Fatalf("removal not persisted: %d rules remain", len(rules3))
	}
}
