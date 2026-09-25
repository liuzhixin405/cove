package permission

import "testing"

// Deny beats allow when both match at the same priority, whatever the order
// they were added in.
func TestPolicyEngine_DenyWinsPriorityTie(t *testing.T) {
	for _, order := range [][]PolicyAction{{ActionAllow, ActionDeny}, {ActionDeny, ActionAllow}} {
		pe := NewPolicyEngine()
		for i, a := range order {
			_ = pe.AddRule(PolicyRule{ID: string(a) + string(rune('0'+i)), ToolPattern: "bash", Action: a, Enabled: true, CommandPrefix: "git push"})
		}
		if got := pe.Evaluate("bash", map[string]any{"command": "git push"}, "default"); got != ActionDeny {
			t.Errorf("order %v: Evaluate = %q, want deny", order, got)
		}
	}
	// A higher-priority allow still beats a lower-priority deny.
	pe := NewPolicyEngine()
	_ = pe.AddRule(PolicyRule{ID: "d", ToolPattern: "bash", Action: ActionDeny, Enabled: true, Priority: 1})
	_ = pe.AddRule(PolicyRule{ID: "a", ToolPattern: "bash", Action: ActionAllow, Enabled: true, Priority: 5})
	if got := pe.Evaluate("bash", nil, "default"); got != ActionAllow {
		t.Errorf("higher-priority allow: Evaluate = %q, want allow", got)
	}
}

// RemoveRules takes out exactly the given rules, one instance each.
func TestManagerRemoveRules(t *testing.T) {
	m := NewManager(Default)
	disk := Rule{ToolPattern: "bash", CommandPrefix: "git commit"}
	m.AddRule(DAllow, disk)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "go test"})
	m.AddRule(DDeny, Rule{ToolPattern: "bash", CommandPrefix: "git push"})
	mcp := Rule{ToolPattern: "mcp", InputEquals: map[string]string{"serverName": "s"}}
	m.AddRule(DAllow, mcp)

	m.RemoveRules(DAllow, []Rule{disk, mcp})
	if d, _ := m.Check("bash", map[string]any{"command": "git commit -m x"}, DAsk); d != DAsk {
		t.Errorf("removed rule still applies: %s", d)
	}
	if d, _ := m.Check("mcp", map[string]any{"serverName": "s"}, DAsk); d != DAsk {
		t.Errorf("removed InputEquals rule still applies: %s", d)
	}
	if d, _ := m.Check("bash", map[string]any{"command": "go test ./..."}, DAsk); d != DAllow {
		t.Errorf("unrelated allow rule removed: %s", d)
	}
	// The deny list is untouched by an allow removal.
	m.RemoveRules(DAllow, []Rule{{ToolPattern: "bash", CommandPrefix: "git push"}})
	if d, _ := m.Check("bash", map[string]any{"command": "git push"}, DAsk); d != DDeny {
		t.Errorf("deny rule removed by an allow removal: %s", d)
	}
	m.RemoveRules(DDeny, []Rule{{ToolPattern: "bash", CommandPrefix: "git push"}})
	if d, _ := m.Check("bash", map[string]any{"command": "git push"}, DAsk); d != DAsk {
		t.Errorf("deny rule not removed: %s", d)
	}
}

// Among rules of the same priority an ask beats an allow, whatever order they
// were written in (deny still beats both).
func TestEvaluateAskBeatsAllowAtSamePriority(t *testing.T) {
	pe := NewPolicyEngine()
	pe.LoadRules([]PolicyRule{
		{ID: "a", ToolPattern: "bash", Action: ActionAllow, Enabled: true, CommandPrefix: "git commit"},
		{ID: "b", ToolPattern: "bash", Action: ActionAsk, Enabled: true, CommandPrefix: "git commit"},
	})
	if got := pe.Evaluate("bash", map[string]any{"command": "git commit -m x"}, "default"); got != ActionAsk {
		t.Fatalf("allow+ask = %v, want ask", got)
	}
	pe.LoadRules([]PolicyRule{
		{ID: "a", ToolPattern: "bash", Action: ActionAllow, Enabled: true, CommandPrefix: "git commit", Priority: 5},
		{ID: "b", ToolPattern: "bash", Action: ActionAsk, Enabled: true, CommandPrefix: "git commit"},
	})
	if got := pe.Evaluate("bash", map[string]any{"command": "git commit -m x"}, "default"); got != ActionAllow {
		t.Fatalf("higher-priority allow = %v, want allow", got)
	}
}
