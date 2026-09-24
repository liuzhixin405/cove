package cost

import (
	"os"
	"path/filepath"
	"testing"
)

func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// Save wrote {"records": [...]} while load expected a bare array, so every
// restart failed to parse the file, reset the history to empty, and the next
// Save threw the old records away: /cost's 24h / 7d / all-time totals never
// covered more than the current process.
func TestCostHistorySurvivesARestart(t *testing.T) {
	isolateHome(t)
	tr := NewTracker(0)
	tr.AddDetailed("deepseek-v4-pro", 1_000_000, 0, 0, 0)

	h := NewCostHistory()
	h.Add("s1", "deepseek-v4-pro", tr)
	if err := h.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again := NewCostHistory()
	if len(again.Records) != 1 || again.Records[0].SessionID != "s1" {
		t.Fatalf("records after reload = %+v, want the saved session", again.Records)
	}
	if got := again.TotalAllTime(); got < 1.31 || got > 1.33 {
		t.Fatalf("TotalAllTime = %v, want the saved $1.32", got)
	}
}

// Files written by the old Save are {"records": [...]}; they must still load.
func TestCostHistoryReadsTheOldObjectFormat(t *testing.T) {
	home := isolateHome(t)
	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"records":[{"session_id":"old","model":"m","cost":0.5,"timestamp":"2026-09-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, "cost_history.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewCostHistory()
	if len(h.Records) != 1 || h.Records[0].SessionID != "old" {
		t.Fatalf("records = %+v, want the one from the old-format file", h.Records)
	}
}

// Save must replace the file atomically: a crash mid-write used to leave a
// truncated file, which the next load discarded along with all history.
func TestCostHistorySaveLeavesNoTempFiles(t *testing.T) {
	home := isolateHome(t)
	h := NewCostHistory()
	h.Add("s", "m", NewTracker(0))
	if err := h.Save(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".cove"))
	for _, e := range entries {
		if e.Name() != "cost_history.json" {
			t.Errorf("unexpected file %q", e.Name())
		}
	}
}
