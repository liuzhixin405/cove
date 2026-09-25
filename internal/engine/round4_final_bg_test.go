package engine

import (
	"context"
	"testing"
	"time"
)

// Important 3: BackgroundPending is what the interactive exit checks before
// it waits: true while the turn's extraction runs, false once it is done.
func TestBackgroundPendingDuringExtraction(t *testing.T) {
	eng, xp := extractingEngine(t, 300*time.Millisecond)
	if eng.BackgroundPending() {
		t.Fatal("pending before any turn")
	}
	if _, err := run(t, eng, "我们用 Go 1.25"); err != nil {
		t.Fatal(err)
	}
	if !eng.BackgroundPending() {
		t.Fatal("extraction running, BackgroundPending = false")
	}
	eng.WaitBackground(context.Background())
	if eng.BackgroundPending() || !xp.finished.Load() {
		t.Fatalf("after the wait: pending=%v finished=%v", eng.BackgroundPending(), xp.finished.Load())
	}
}
