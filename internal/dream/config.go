package dream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/log"
)

// Trigger modes (dream.json "trigger").
const (
	// TriggerSessionEnd consolidates when a conversation ends: the exit path
	// starts a detached `cove --dream-worker` once the session had at least
	// MinTurns assistant turns. The per-turn check (ExecuteAutoDream) is off.
	TriggerSessionEnd = "session_end"
	// TriggerThreshold is the former behaviour: the per-turn check runs a
	// consolidation once MinHours and MinSessions are both met.
	TriggerThreshold = "threshold"
)

// Config holds auto-dream scheduling settings.
type Config struct {
	Enabled bool `json:"enabled"`
	// Trigger is TriggerSessionEnd (default) or TriggerThreshold.
	Trigger string `json:"trigger"`
	// MinTurns is how many assistant turns a session needs before its end
	// starts a consolidation (session_end mode).
	MinTurns int `json:"min_turns"`
	// MinIntervalMinutes (session_end mode) is the least time between two
	// consolidations; 0, the default, means no minimum.
	MinIntervalMinutes int `json:"min_interval_minutes"`
	// MinHours and MinSessions are the threshold-mode gates.
	MinHours    int `json:"min_hours"`
	MinSessions int `json:"min_sessions"`
}

// defaults: consolidate when a conversation of at least two assistant turns
// ends. Nobody works around the clock, so waiting for 12 hours and three
// other sessions meant most users hardly ever saw a consolidation. The
// threshold gates (12h / 3 sessions) still apply with "trigger": "threshold".
var defaults = Config{
	Enabled:     true,
	Trigger:     TriggerSessionEnd,
	MinTurns:    2,
	MinHours:    12,
	MinSessions: 3,
}

// configDir is where dream.json, dream.log and dream-last.json live: the
// config directory (COVE_CONFIG_DIR, else ~/.cove).
func configDir() string {
	if d, err := config.ConfigDir(); err == nil {
		return d
	}
	return filepath.Join(homeDir(), ".cove")
}

// LoadConfig reads dream config from dream.json in the config directory,
// falling back to defaults.
func LoadConfig() Config {
	cfg := defaults
	path := filepath.Join(configDir(), "dream.json")
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
	if cfg.MinTurns <= 0 {
		cfg.MinTurns = defaults.MinTurns
	}
	if cfg.MinIntervalMinutes < 0 {
		cfg.MinIntervalMinutes = 0
	}
	switch t := strings.ToLower(strings.TrimSpace(cfg.Trigger)); t {
	case TriggerSessionEnd, TriggerThreshold:
		cfg.Trigger = t
	case "":
		cfg.Trigger = defaults.Trigger
	default:
		log.Warnf("[dream] %s: unknown trigger %q (want %q or %q); using %q", path, cfg.Trigger, TriggerSessionEnd, TriggerThreshold, defaults.Trigger)
		cfg.Trigger = defaults.Trigger
	}
	return cfg
}

// IsEnabled checks if auto-dream is turned on.
func IsEnabled() bool {
	return LoadConfig().Enabled
}
