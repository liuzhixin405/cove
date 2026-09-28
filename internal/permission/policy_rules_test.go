package permission

import "testing"

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
