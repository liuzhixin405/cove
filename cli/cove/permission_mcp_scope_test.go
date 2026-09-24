package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/permission"
)

// "a" for the MCP proxy tool used to add {ToolPattern: "mcp"}, which allowed
// every tool on every connected server, destructive ones included, for the
// rest of the session.
func TestAlwaysAllowScopeForMCPIsOneServerTool(t *testing.T) {
	rules, scope, ok := alwaysAllowScope("mcp", map[string]any{"serverName": "github", "toolName": "create_issue"})
	if !ok || len(rules) != 1 {
		t.Fatalf("rules=%v ok=%v", rules, ok)
	}
	if !strings.Contains(scope, "github/create_issue") {
		t.Fatalf("scope %q does not name the server tool", scope)
	}
	m := permission.NewManager(permission.Default)
	m.AddRule(permission.DAllow, rules[0])
	if d, _ := m.Check("mcp", map[string]any{"serverName": "github", "toolName": "create_issue"}, permission.DAsk); d != permission.DAllow {
		t.Fatalf("remembered tool not allowed: %v", d)
	}
	if d, _ := m.Check("mcp", map[string]any{"serverName": "github", "toolName": "delete_repo"}, permission.DAsk); d == permission.DAllow {
		t.Fatal("another tool on the same server was allowed")
	}
}

func TestAlwaysAllowScopeForMCPWithoutNamesIsNotOffered(t *testing.T) {
	if _, _, ok := alwaysAllowScope("mcp", map[string]any{"serverName": "github"}); ok {
		t.Fatal("a call without toolName must not offer a remembered rule")
	}
}
