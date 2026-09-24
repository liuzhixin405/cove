package command

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/mcp"
)

// TestMcpListShowsWhyAServerFailed: connection errors are only logged at debug
// level, so /mcp list was the one place a user could learn why a server is
// down, and it printed "connected=false" without the reason.
func TestMcpListShowsWhyAServerFailed(t *testing.T) {
	pool := &fakeMCPPool{servers: []*mcp.ManagedServer{
		{Name: "files", Err: "connect files: exec: \"npx\": executable file not found in %PATH%"},
	}}
	out, err := NewMcpCmd().Execute(context.Background(), Input{Args: []string{"list"}, MCPPool: pool})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Message, "executable file not found") {
		t.Errorf("list output hides the failure reason: %q", out.Message)
	}
}

// TestMcpDisconnectUnknownServerSaysSo: a typo in the name used to answer
// "已断开 MCP 服务器: <typo>" while the real server stayed connected.
func TestMcpDisconnectUnknownServerSaysSo(t *testing.T) {
	pool := &fakeMCPPool{servers: []*mcp.ManagedServer{{Name: "files", Connected: true}}}
	out, err := NewMcpCmd().Execute(context.Background(), Input{Args: []string{"disconnect", "fiels"}, MCPPool: pool})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.Message, "已断开") {
		t.Errorf("disconnecting an unknown server reported success: %q", out.Message)
	}
	if !strings.Contains(out.Message, "fiels") {
		t.Errorf("message does not name the unknown server: %q", out.Message)
	}
}

// TestPluginInstallDerivesNameFromAnyGitURL covers `/plugin install <url>` for
// the URL forms git accepts. Only https:// and git@ were recognised: an
// http:// or ssh:// URL was treated as a plugin name and looked up in the
// marketplace, and a trailing slash left an empty name that failed validation.
func TestPluginInstallDerivesNameFromAnyGitURL(t *testing.T) {
	cases := []struct {
		url, name string
	}{
		{"https://github.com/acme/linter.git", "linter"},
		{"https://github.com/acme/linter/", "linter"},
		{"http://git.corp.local/team/linter", "linter"},
		{"ssh://git@git.corp.local:2222/team/linter.git", "linter"},
		{"git@github.com:acme/linter.git", "linter"},
		{"git@github.com:linter.git", "linter"},
	}
	for _, tc := range cases {
		pm := &fakePluginManager{}
		_, err := NewPluginCmd().Execute(context.Background(), Input{Args: []string{"install", tc.url}, PluginManager: pm})
		if err != nil {
			t.Errorf("%s: Execute error = %v", tc.url, err)
			continue
		}
		want := tc.name + "|" + tc.url
		if len(pm.installs) != 1 || pm.installs[0] != want {
			t.Errorf("%s: installs = %v, want [%s]", tc.url, pm.installs, want)
		}
	}
}
