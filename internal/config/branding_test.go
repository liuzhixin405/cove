package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoveBranding_NoLegacyClaudeNamesInDemoTree(t *testing.T) {
	demoRoot := filepath.Clean(filepath.Join("..", ".."))
	// Identifier-style legacy names only. The spaced product name "Claude Code"
	// is intentionally not scanned: the honest-comparison docs (README,
	// docs/COMPARISON.md, docs/WHY_GO.md) reference it as a third-party product.
	legacyTokens := []string{
		"claude" + "-code",
		"claude" + "-code-go",
		"github.com/" + "claude" + "-code-go",
		"." + "claude" + "-code-go",
		"cmd/" + "claude" + "-code",
	}
	allowedExt := map[string]bool{
		".go":   true,
		".md":   true,
		".txt":  true,
		".json": true,
		".yml":  true,
		".yaml": true,
		".py":   true,
		".bat":  true,
		".mod":  true,
	}
	ignoredFiles := map[string]bool{
		"go.sum": true,
	}

	var hits []string
	walkErr := filepath.WalkDir(demoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(demoRoot, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == "." {
			return nil
		}

		lowerRel := strings.ToLower(relSlash)
		for _, token := range legacyTokens {
			if strings.Contains(lowerRel, token) {
				hits = append(hits, "path: "+relSlash)
				if d.IsDir() {
					return filepath.SkipDir
				}
				break
			}
		}

		if d.IsDir() {
			switch relSlash {
			case ".git", "dist", "vendor":
				return filepath.SkipDir
			case "docs/superpowers", ".superpowers":
				// research/plan documents legitimately mention other products.
				return filepath.SkipDir
			}
			return nil
		}

		if ignoredFiles[filepath.Base(relSlash)] {
			return nil
		}
		if !allowedExt[filepath.Ext(relSlash)] {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := strings.ToLower(string(data))
		for _, token := range legacyTokens {
			if strings.Contains(text, token) {
				hits = append(hits, "content: "+relSlash)
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk demo tree: %v", walkErr)
	}
	if len(hits) > 0 {
		t.Fatalf("found legacy naming in demo tree:\n%s", strings.Join(hits, "\n"))
	}
}

func TestConfigDirAndProjectOverrideUseCoveNames(t *testing.T) {
	tmp := t.TempDir()
	oldHome := os.Getenv("HOME")
	oldUserProfile := os.Getenv("USERPROFILE")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.Setenv("USERPROFILE", oldUserProfile)
		_ = os.Chdir(oldWD)
	})
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("USERPROFILE", tmp)

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir error: %v", err)
	}
	wantDir := filepath.Join(tmp, ".cove")
	if dir != wantDir {
		t.Fatalf("ConfigDir = %q, want %q", dir, wantDir)
	}

	projectDir := filepath.Join(tmp, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".cove.json"), []byte(`{"model":"gpt-4o","permission_mode":"auto"}`), 0644); err != nil {
		t.Fatalf("write override: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	// permission_mode "auto" loosens the default, so .cove.json may only set
	// it once trusted; trust it here, this test is about the file names.
	if err := TrustProjectConfig(filepath.Join(projectDir, ".cove.json")); err != nil {
		t.Fatalf("trust: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Model != "gpt-4o" {
		t.Fatalf("expected override model, got %q", cfg.Model)
	}
	if cfg.PermissionMode != "auto" {
		t.Fatalf("expected override permission mode, got %q", cfg.PermissionMode)
	}
}
