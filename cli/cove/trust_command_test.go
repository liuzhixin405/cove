package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/permission"
)

// A project's .cove.json that would start an MCP server is ignored until
// /trust, and startup says so: it used to apply in full, so a cloned
// repository could run a command at launch.
func TestTrustCommandEnablesProjectConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	proj := t.TempDir()
	t.Chdir(proj)
	body := `{"mcp_servers":{"x":{"command":"python","args":["-c","print(1)"]}}}`
	if err := os.WriteFile(filepath.Join(proj, ".cove.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 0 {
		t.Fatalf("untrusted mcp_servers applied: %v", cfg.MCPServers)
	}
	notice := untrustedProjectNotice(cfg)
	if !strings.Contains(notice, "mcp_servers") || !strings.Contains(notice, "/trust") {
		t.Fatalf("startup notice = %q", notice)
	}

	if msg := trustProjectConfig(cfg); !strings.Contains(msg, "已信任") || !strings.Contains(msg, "/restart") {
		t.Fatalf("/trust said %q", msg)
	}
	cfg, err = config.LoadWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 || untrustedProjectNotice(cfg) != "" {
		t.Fatalf("after /trust: servers=%v notice=%q", cfg.MCPServers, untrustedProjectNotice(cfg))
	}
	// Trusting the file trusted its directory too.
	if msg := trustProjectConfig(cfg); msg != "此项目已受信任。" {
		t.Fatalf("/trust with nothing to trust said %q", msg)
	}

	// Any edit drops the trust.
	if err := os.WriteFile(filepath.Join(proj, ".cove.json"), []byte(strings.Replace(body, "print(1)", "print(2)", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.LoadWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 0 || untrustedProjectNotice(cfg) == "" {
		t.Fatalf("an edited file kept its trust: servers=%v", cfg.MCPServers)
	}
}

// After /cd the loaded .cove.json is the startup directory's. /trust used
// to trust it and point at /restart, and the restarted cove then found the
// new directory's file still untrusted.
func TestTrustRefusedAfterWorkingDirectoryChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	proj := t.TempDir()
	t.Chdir(proj)
	body := `{"mcp_servers":{"x":{"command":"python","args":["-c","print(1)"]}}}`
	if err := os.WriteFile(filepath.Join(proj, ".cove.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // /cd
	msg := trustProjectConfig(cfg)
	if strings.Contains(msg, "已信任") || !strings.Contains(msg, "/restart") || !strings.Contains(msg, "工作目录已切换") {
		t.Fatalf("/trust after /cd said %q", msg)
	}
	if ok, _ := config.IsProjectConfigTrusted(filepath.Join(proj, ".cove.json")); ok {
		t.Fatal("/trust after /cd trusted the startup directory's .cove.json")
	}
}

// A project without .cove.json still needs a trust decision before cove runs
// its detected build and tests unasked (npm run build executes a
// package.json script). /trust there used to say there was nothing to trust,
// leaving no way to turn the automatic verification on.
func TestTrustCommandTrustsDirectoryWithoutProjectConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	proj := t.TempDir()
	t.Chdir(proj)
	cfg, err := config.LoadWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	msg := trustProjectConfig(cfg)
	if !strings.Contains(msg, "已信任此项目目录") || !strings.Contains(msg, "自动运行构建/测试校验") {
		t.Fatalf("/trust said %q", msg)
	}
	root := permission.ProjectRoot(proj)
	if ok, _ := config.IsProjectDirTrusted(root); !ok {
		t.Fatalf("/trust did not trust %s", root)
	}
	if msg := trustProjectConfig(cfg); msg != "此项目已受信任。" {
		t.Fatalf("second /trust said %q", msg)
	}
}
