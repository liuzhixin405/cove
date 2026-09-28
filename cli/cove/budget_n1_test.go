package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/config"
)

// "/budget <n>" changes this session only; "/budget save" writes it; and
// "/budget off" removes the cap.
func TestBudgetCommandSessionOnlyUntilSave(t *testing.T) {
	eng := newTestEngine(t)
	buf := captureOut(t)
	cfg := config.DefaultConfig()
	cfg.MaxBudgetUsd = 10
	cfgPath := filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json")

	handleBudgetCommand("/budget 3", cfg, eng)
	if got := eng.CostTracker().Totals().MaxBudget; got != 3 {
		t.Fatalf("session budget = %v, want 3", got)
	}
	if cfg.MaxBudgetUsd != 10 {
		t.Fatalf("/budget 3 changed the config to %v", cfg.MaxBudgetUsd)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatalf("/budget 3 wrote the config file")
	}

	handleBudgetCommand("/budget save", cfg, eng)
	if cfg.MaxBudgetUsd != 3 || !strings.Contains(savedConfig(t), `"max_budget_usd": 3`) {
		t.Fatalf("/budget save: cfg %v, file %s", cfg.MaxBudgetUsd, savedConfig(t))
	}

	handleBudgetCommand("/budget off", cfg, eng)
	if got := eng.CostTracker().Totals().MaxBudget; got != 0 || eng.CostTracker().OverBudget() {
		t.Fatalf("/budget off left cap %v", got)
	}
	if !strings.Contains(buf.String(), "取消") {
		t.Fatalf("no confirmation for off: %q", buf.String())
	}
}
