package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// TestValidatePluginNameRejectsTraversal is the regression test for the path
// traversal in the install paths. Plugin names are joined into a directory that
// is git-cloned into and os.RemoveAll'd on failure, and they arrive from the
// `name` field of manifests inside a cloned REMOTE marketplace repository — so
// an entry naming itself "../../../.ssh" redirected both operations outside the
// plugins directory.
func TestValidatePluginNameRejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"../evil",
		"../../../.ssh",
		"..\\..\\evil",
		"a/b",
		`a\b`,
		"/etc/passwd",
		`C:\Windows`,
		`\\server\share`,
		".hidden",
		".git",
		"name\x00null",
		"has space",
		"semi;colon",
		"star*",
		"pipe|x",
		"quote\"x",
		"tilde~x",
		"dollar$x",
		"myplugin.disabled",
		strings.Repeat("a", maxPluginNameLen+1),
	}
	for _, name := range bad {
		if err := ValidatePluginName(name); err == nil {
			t.Errorf("ValidatePluginName(%q) = nil, want an error", name)
		}
	}
}

// TestValidatePluginNameAcceptsRealNames guards against the check being so
// strict that ordinary plugin names stop working.
func TestValidatePluginNameAcceptsRealNames(t *testing.T) {
	good := []string{
		"myplugin",
		"my-plugin",
		"my_plugin",
		"my.plugin",
		"cove-linter",
		"Plugin2",
		"a",
		strings.Repeat("a", maxPluginNameLen),
	}
	for _, name := range good {
		if err := ValidatePluginName(name); err != nil {
			t.Errorf("ValidatePluginName(%q) = %v, want nil", name, err)
		}
	}
}

// TestPluginDirForStaysUnderRoot asserts the property that matters: whatever
// pluginDirFor returns is inside the root it was given.
func TestPluginDirForStaysUnderRoot(t *testing.T) {
	root := filepath.Join("home", "u", ".cove", "plugins")

	if _, err := pluginDirFor(root, "../../../.ssh"); err == nil {
		t.Fatal("pluginDirFor accepted a traversal name")
	}

	dir, err := pluginDirFor(root, "linter")
	if err != nil {
		t.Fatalf("pluginDirFor: %v", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if rel != "linter" {
		t.Fatalf("resolved to %q (rel %q), want it directly under the root", dir, rel)
	}
}

// TestFetchFileSourceDropsHostileEntries covers the index-level filter: a
// registry.json entry with an unusable name must never enter the index.
func TestFetchFileSourceDropsHostileEntries(t *testing.T) {
	tmp := t.TempDir()
	registry := filepath.Join(tmp, "registry.json")
	content := `[
	  {"name":"good-plugin","source":"https://github.com/x/y.git"},
	  {"name":"../../../.ssh","source":"https://github.com/x/evil.git"},
	  {"name":"a/b","source":"https://github.com/x/evil2.git"},
	  {"name":"","source":"https://github.com/x/evil3.git"}
	]`
	if err := writeFileForTest(registry, content); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	m := &Marketplace{}
	entries, err := m.fetchFileSource(registry)
	if err != nil {
		t.Fatalf("fetchFileSource: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (only the valid one)", len(entries))
	}
	if entries[0].Name != "good-plugin" {
		t.Fatalf("kept %q, want %q", entries[0].Name, "good-plugin")
	}
}
