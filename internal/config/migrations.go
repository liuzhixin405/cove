package config

import (
	"fmt"
	"reflect"
)

const CurrentVersion = 1

type migrateRule struct {
	Version int
	Apply   func(cfg *Config) error
}

var migrateRules = []migrateRule{
	{Version: 1, Apply: func(cfg *Config) error {
		if cfg.ThinkingTokens < 1024 {
			cfg.ThinkingTokens = 16000
		}
		if cfg.PermissionMode == "" {
			cfg.PermissionMode = "default"
		}
		return nil
	}},
}

func Migrate(cfg *Config, fromVersion int) error {
	if fromVersion < 0 {
		fromVersion = 0
	}
	if fromVersion >= CurrentVersion {
		return nil
	}

	before := *cfg
	changed := false
	for _, m := range migrateRules {
		if m.Version > fromVersion {
			if err := m.Apply(cfg); err != nil {
				return fmt.Errorf("migration v%d: %w", m.Version, err)
			}
			if !reflect.DeepEqual(before, *cfg) {
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return Save(cfg)
}
