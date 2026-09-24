package permission

import (
	"sync"
	"testing"
)

// Answering "a" (always allow bash) in default mode adds a session allow rule.
// Switching to plan mode afterwards must still refuse writes: plan mode is the
// read-only promise, not a default that rules can override.
func TestPlanModeIgnoresAllowRulesForNonReadOnlyTools(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "bash"})
	m.SetMode(Plan)

	if d, _ := m.Check("bash", map[string]any{"command": "rm -rf build"}, DAsk); d != DDeny {
		t.Fatalf("plan mode bash with allow rule = %v, want deny", d)
	}
	if d, _ := m.Check("read", nil, DAllow); d != DAllow {
		t.Fatalf("plan mode read = %v, want allow", d)
	}
}

// Parallel tool calls check permissions while the prompt handler adds rules.
func TestManagerConcurrentAddAndCheck(t *testing.T) {
	m := NewManager(Default)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); m.AddRule(DAllow, Rule{ToolPattern: "bash"}) }()
		go func() { defer wg.Done(); m.Check("bash", nil, DAsk) }()
	}
	wg.Wait()
}
