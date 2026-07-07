package diagnostic

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckConfigExistsCreatesRicherDefaultConfig(t *testing.T) {
	tmpDir := t.TempDir()
	checker := NewChecker(nil)
	checker.homeDir = tmpDir

	res := checker.checkConfigExists(nil)
	if res.Error == nil {
		t.Fatalf("expected config missing error to be auto-fixed")
	}

	cfgPath := filepath.Join(tmpDir, ".cove", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}

	content := string(data)
	if !contains(content, "\"provider\"") || !contains(content, "\"api_key\"") && !contains(content, "\"base_url\"") {
		t.Fatalf("expected richer default config content, got %s", content)
	}
	if contains(content, "\"permission_mode\": \"ask\"") {
		t.Fatalf("did not expect legacy minimal config template, got %s", content)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || containsAt(s, sub, 0))
}

func containsAt(s, sub string, start int) bool {
	if len(sub) == 0 {
		return true
	}
	for i := start; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
