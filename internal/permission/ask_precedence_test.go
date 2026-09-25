package permission

import "testing"

// An ask rule is a request to be asked every time: an allow rule for the same
// command (an earlier "[a]", a persisted "[p]", a whole-tool allow) must not
// silence it.
func TestAskRuleBeatsAllowRule(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DAsk, Rule{ToolPattern: "bash", CommandPrefix: "git status"})
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git status"})
	m.AddRule(DAllow, Rule{ToolPattern: "bash"})
	if d, _ := m.Check("bash", map[string]any{"command": "git status"}, DAllow); d != DAsk {
		t.Fatalf("ask + allow git status = %v, want ask", d)
	}
	if d, _ := m.Check("bash", map[string]any{"command": "git log"}, DAsk); d != DAllow {
		t.Fatalf("allow still applies to other commands: got %v", d)
	}
	// Deny still beats ask.
	m.AddRule(DDeny, Rule{ToolPattern: "bash", CommandPrefix: "git status"})
	if d, _ := m.Check("bash", map[string]any{"command": "git status"}, DAllow); d != DDeny {
		t.Fatalf("deny + ask = %v, want deny", d)
	}
}

// Bypass mode (when available) skips ask rules; only deny rules and plan
// mode stop it.
func TestBypassModeIgnoresAskRules(t *testing.T) {
	m := NewManager(Bypass)
	m.SetBypassAvailable(true)
	m.AddRule(DAsk, Rule{ToolPattern: "bash", CommandPrefix: "git status"})
	if d, _ := m.Check("bash", map[string]any{"command": "git status"}, DAsk); d != DBypass {
		t.Fatalf("bypass + ask rule = %v, want bypass", d)
	}
}
