package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The telemetry setting was removed with the unused recorder. Old config files
// still carry the key, in whatever shape an earlier version or the user wrote
// it; they must keep loading, and Save must not drop the key.
func TestConfigWithTelemetryKeyStillLoads(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-4o","telemetry":{}}`,
		`{"model":"gpt-4o","telemetry":true}`,
	} {
		dir := t.TempDir()
		t.Setenv("COVE_CONFIG_DIR", dir)
		t.Chdir(t.TempDir())
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(%s): %v", body, err)
		}
		if cfg.Model != "gpt-4o" {
			t.Errorf("Load(%s): model = %q, the rest of the file was ignored", body, cfg.Model)
		}
		if err := CheckFile(path); err != nil {
			t.Errorf("CheckFile(%s) = %v", body, err)
		}
		cfg.Effort = "high"
		if err := Save(cfg); err != nil {
			t.Fatalf("Save: %v", err)
		}
		m := readJSONMap(t, path)
		if _, ok := m["telemetry"]; !ok {
			t.Errorf("Save dropped the unknown telemetry key: %v", m)
		}
	}
}
