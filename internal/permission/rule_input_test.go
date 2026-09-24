package permission

import "testing"

// An "always allow" answer for the MCP proxy tool must cover that one server
// tool, not every tool on every connected server.
func TestInputEqualsScopesAllowRule(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "mcp", InputEquals: map[string]string{"serverName": "github", "toolName": "create_issue"}})

	allowed := map[string]any{"serverName": "github", "toolName": "create_issue", "arguments": map[string]any{}}
	if d, _ := m.Check("mcp", allowed, DAsk); d != DAllow {
		t.Fatalf("the remembered server tool: decision %v, want allow", d)
	}
	for _, in := range []map[string]any{
		{"serverName": "github", "toolName": "delete_repo"},
		{"serverName": "other", "toolName": "create_issue"},
		{"toolName": "create_issue"},
		nil,
	} {
		if d, _ := m.Check("mcp", in, DAsk); d != DAsk {
			t.Errorf("input %v: decision %v, want ask", in, d)
		}
	}
}
