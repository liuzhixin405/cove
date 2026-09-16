package dream

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/liuzhixin405/cove/internal/log"
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

// LoadConfig reads dream config from ~/.cove/dream.json, falling back to defaults.
func LoadConfig() Config {
	cfg := defaults
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}
	path := filepath.Join(home, ".cove", "dream.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// A malformed file must not silently fall back to Enabled: true. cfg is
		// partially overwritten by a failed Unmarshal, and the default has
		// consolidation ON — so a user who had turned it off and then hit a
		// typo in their config would have it quietly turned back on, with no
		// message anywhere. Report it and use the pristine defaults.
		log.Warnf("[dream] %s is not valid JSON (%v); using defaults", path, err)
		return defaults
	}
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
