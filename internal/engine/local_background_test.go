package engine

import (
	"context"
	"testing"
	"time"
)

// A local server answers one request at a time: the memory extraction,
// review and dream calls after every turn queued the user's next turn
// behind them (and timed out at 30s on a 27B model, filling errors.log
// with "[extractMemories] context deadline exceeded"). With a local
// provider the per-turn learning calls are skipped.
func TestLocalProviderSkipsPerTurnBackgroundModelCalls(t *testing.T) {
	eng, xp := extractingEngine(t, 20*time.Millisecond)
	eng.config.Provider.BaseURL = "http://127.0.0.1:8080/v1"
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	eng.WaitBackground(ctx)
	if xp.finished.Load() {
		t.Fatal("memory extraction called the local model after the turn")
	}
}

// The same engine against a remote provider still extracts (the control
// for the test above).
func TestRemoteProviderStillExtractsAfterTheTurn(t *testing.T) {
	eng, xp := extractingEngine(t, 20*time.Millisecond)
	eng.config.Provider.BaseURL = "https://api.deepseek.com/v1"
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	eng.WaitBackground(ctx)
	if !xp.finished.Load() {
		t.Fatal("memory extraction did not run for a remote provider")
	}
}
