package diagnostic

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove/internal/config"
)

// This file used to hold TestCheckConfigExistsCreatesRicherDefaultConfig,
// which required the check to write a config.json with a placeholder API key.
// That write was the bug (see checker_audit_test.go): it replaced the user's
// environment key with "sk-xxxx…" from the next launch on. The test now pins
// the opposite: a missing config is reported, never created.
func TestCheckConfigExistsDoesNotCreateAConfig(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "deepseek"}})

	_ = c.checkConfigExists(t.Context())

	if _, err := os.Stat(filepath.Join(c.configDir, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config.json was created (err=%v)", err)
	}
}
