package dream

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds auto-dream scheduling thresholds.
type Config struct {
	Enabled     bool `json:"enabled"`
	MinHours    int  `json:"min_hours"`
	MinSessions int  `json:"min_sessions"`
}

var defaults = Config{
	Enabled:     true,
	MinHours:    24,
	MinSessions: 5,
}

// LoadConfig reads dream config from ~/.agentgo/dream.json, falling back to defaults.
func LoadConfig() Config {
	cfg := defaults
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(filepath.Join(home, ".agentgo", "dream.json"))
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	if cfg.MinHours <= 0 {
		cfg.MinHours = defaults.MinHours
	}
	if cfg.MinSessions <= 0 {
		cfg.MinSessions = defaults.MinSessions
	}
	return cfg
}

// IsEnabled checks if auto-dream is turned on.
func IsEnabled() bool {
	return LoadConfig().Enabled
}
