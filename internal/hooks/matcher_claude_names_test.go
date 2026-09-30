package hooks

import "testing"

// Matchers were anchored and case-sensitive, while cove's tool names are
// lowercase, so a Claude Code-style hooks.json ("matcher": "Bash",
// "Edit|Write") loaded without error and then never fired.
func TestConfigMatcherAcceptsClaudeCodeToolNames(t *testing.T) {
	cases := []struct {
		matcher, tool string
		want          bool
	}{
		{"Bash", "bash", true},
		{"Bash", "powershell", true},
		{"BASH", "bash", true},
		{"Edit|Write", "edit", true},
		{"Edit|Write", "write", true},
		{"Edit|Write", "read", false},
		{"MultiEdit", "edit", true},
		{"Read", "read", true},
		{"Grep", "grep", true},
		{"Glob", "glob", true},
		{"WebFetch", "webfetch", true},
		{"WebSearch", "websearch", true},
		{"Bash", "bash_output", false},
		{"Bash", "edit", false},
		{"PowerShell", "powershell", true},
		{"PowerShell", "bash", false},
	}
	m := NewManager()
	for _, c := range cases {
		h := HookDef{Event: BeforeTool, Matcher: c.matcher, Command: "x"}.Config()
		if got := m.matches(h, c.tool); got != c.want {
			t.Errorf("matcher %q vs %q = %v, want %v (compiled %q)", c.matcher, c.tool, got, c.want, h.Matcher)
		}
	}
}
