package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateAllReportsPluginsItCannotUpdate: a plugin installed from the
// marketplace cache is a plain copy, not a git checkout, so `git pull` cannot
// update it. `/plugin update` used to skip it silently and answer "所有插件已是最新",
// which is false: the user never learns that the plugin will never update.
func TestUpdateAllReportsPluginsItCannotUpdate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	mgr := NewManager()
	mgr.Init()

	cached := filepath.Join(tmp, ".cove", "marketplace", "cache", "official", "plugins", "foo", ".claude-plugin")
	if err := os.MkdirAll(cached, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cached, "plugin.json"), []byte(`{"name":"foo","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.Marketplace().index = []MarketplaceEntry{{Name: "foo", Source: "https://example.invalid/foo.git"}}
	if err := mgr.MarketplaceInstall("foo"); err != nil {
		t.Fatalf("MarketplaceInstall: %v", err)
	}

	msg, err := mgr.MarketplaceUpdate("")
	if err != nil {
		t.Fatalf("MarketplaceUpdate: %v", err)
	}
	if strings.Contains(msg, "所有插件已是最新") {
		t.Errorf("update claims everything is current although foo cannot be updated: %q", msg)
	}
	if !strings.Contains(msg, "foo") {
		t.Errorf("update message does not name the plugin it skipped: %q", msg)
	}
}
