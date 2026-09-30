package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	mgr := NewManager()
	mgr.Init()
	return mgr, tmp
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A plugin that failed to load is recorded with State=Error and its own
// directory (no ".disabled" suffix). Enable trimmed the suffix — a no-op — and
// then os.RemoveAll'd the "enabled" directory, which was the plugin itself.
func TestEnableErrorPluginDoesNotDeleteIt(t *testing.T) {
	mgr, _ := newTestManager(t)
	bad := filepath.Join(mgr.dir, "broken")
	mustWrite(t, filepath.Join(bad, "notes.txt"), "the user's work")
	mgr.Refresh()

	err := mgr.Enable("broken")
	if err == nil || !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "加载失败") {
		t.Fatalf("Enable = %v, want a load-failure error naming the plugin", err)
	}
	if _, statErr := os.Stat(filepath.Join(bad, "notes.txt")); statErr != nil {
		t.Fatalf("Enable deleted the plugin directory: %v", statErr)
	}
}

// A disabled plugin that failed to load has a ".disabled" directory but
// State=Error; Enable must not rename it into place either.
func TestEnableDisabledErrorPluginKeepsIt(t *testing.T) {
	mgr, _ := newTestManager(t)
	bad := filepath.Join(mgr.dir, "broken.disabled")
	mustWrite(t, filepath.Join(bad, "notes.txt"), "x")
	mgr.Refresh()
	if err := mgr.Enable("broken"); err == nil {
		t.Fatal("Enable of a plugin that failed to load succeeded")
	}
	if _, err := os.Stat(filepath.Join(bad, "notes.txt")); err != nil {
		t.Fatalf("plugin directory gone: %v", err)
	}
}

// CommandPrompts promised "first writer wins" but ranged over a map, so which
// plugin's /x ran changed from one start to the next.
func TestCommandPromptsIsDeterministic(t *testing.T) {
	mgr, _ := newTestManager(t)
	for _, name := range []string{"zeta", "alpha", "mid"} {
		dir := filepath.Join(mgr.dir, name)
		mustWrite(t, filepath.Join(dir, "manifest.json"), `{"name":"`+name+`"}`)
		mustWrite(t, filepath.Join(dir, "commands", "x.md"), "from "+name)
	}
	mgr.Refresh()
	for i := 0; i < 50; i++ {
		got := mgr.CommandPrompts()["x"]
		if got.Plugin != "alpha" || got.Prompt != "from alpha" {
			t.Fatalf("run %d: /x from %q (%q), want alpha every time", i, got.Plugin, got.Prompt)
		}
	}
}

// The index lookup is case-insensitive and installs into the entry's own
// directory name, but the reload used the name as typed: `/plugin install Foo`
// for entry "foo" reported failure after a successful install on a
// case-sensitive file system (and recorded the wrong directory elsewhere).
func TestMarketplaceInstallUsesTheIndexEntryName(t *testing.T) {
	mgr, tmp := newTestManager(t)
	cached := filepath.Join(tmp, ".cove", "marketplace", "cache", "official", "plugins", "foo")
	mustWrite(t, filepath.Join(cached, ".claude-plugin", "plugin.json"), `{"name":"foo","version":"1.0.0"}`)
	mgr.Marketplace().index = []MarketplaceEntry{{Name: "foo", Source: defaultMarketplaceRepo, Marketplace: "official", Path: "plugins/foo"}}

	if err := mgr.MarketplaceInstall("Foo"); err != nil {
		t.Fatalf("MarketplaceInstall: %v", err)
	}
	var found *Entry
	for _, e := range mgr.AllPlugins() {
		if e.Manifest.Name == "foo" {
			e := e
			found = &e
		}
	}
	if found == nil || filepath.Base(found.Dir) != "foo" {
		t.Fatalf("installed plugin = %+v, want it loaded from dir foo", found)
	}
}

// findCachedPlugin scanned every cached marketplace and matched directories by
// the entry's name, so an install could copy another marketplace's plugin of
// the same name while the lockfile recorded this entry's source.
func TestMarketplaceInstallCopiesFromTheEntrysOwnMarketplace(t *testing.T) {
	mgr, tmp := newTestManager(t)
	cache := filepath.Join(tmp, ".cove", "marketplace", "cache")
	// "aaa-other" sorts first and used to win.
	mustWrite(t, filepath.Join(cache, "aaa-other", "plugins", "foo", ".claude-plugin", "plugin.json"), `{"name":"foo"}`)
	mustWrite(t, filepath.Join(cache, "aaa-other", "plugins", "foo", "marker"), "other")
	// The entry's plugin lives in a directory whose name differs from its
	// manifest name.
	mustWrite(t, filepath.Join(cache, "official", "plugins", "foo-dir", ".claude-plugin", "plugin.json"), `{"name":"foo"}`)
	mustWrite(t, filepath.Join(cache, "official", "plugins", "foo-dir", "marker"), "official")
	mgr.Marketplace().index = []MarketplaceEntry{{Name: "foo", Source: defaultMarketplaceRepo, Marketplace: "official", Path: "plugins/foo-dir"}}

	if err := mgr.MarketplaceInstall("foo"); err != nil {
		t.Fatalf("MarketplaceInstall: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(mgr.dir, "foo", "marker"))
	if err != nil || string(b) != "official" {
		t.Fatalf("installed marker = %q (%v), want the official marketplace's copy", b, err)
	}
}

// An entry's path comes from the index file; it must not reach outside its
// marketplace's cache directory.
func TestFindCachedPluginRejectsPathEscape(t *testing.T) {
	mgr, tmp := newTestManager(t)
	cache := filepath.Join(tmp, ".cove", "marketplace", "cache")
	mustWrite(t, filepath.Join(cache, "aaa-other", "plugins", "foo", "manifest.json"), `{"name":"foo"}`)
	e := MarketplaceEntry{Name: "foo", Marketplace: "official", Path: "../aaa-other/plugins/foo"}
	if got := mgr.Marketplace().findCachedPlugin(&e); got != "" {
		t.Fatalf("findCachedPlugin = %q, want no match outside the marketplace", got)
	}
}

// The index records which marketplace and directory each entry came from.
func TestFetchClaudePluginsRepoRecordsMarketplaceAndPath(t *testing.T) {
	mgr, tmp := newTestManager(t)
	repo := filepath.Join(tmp, "repo")
	mustWrite(t, filepath.Join(repo, "external_plugins", "bar-dir", ".claude-plugin", "plugin.json"), `{"name":"bar"}`)
	entries, err := mgr.Marketplace().fetchClaudePluginsRepo(repo, MarketplaceSource{Name: "official", URL: "u"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if entries[0].Marketplace != "official" || entries[0].Path != "external_plugins/bar-dir" {
		t.Fatalf("entry = %+v", entries[0])
	}
}
