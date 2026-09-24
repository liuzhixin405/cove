package extract

import (
	"context"
	"testing"
)

// The first turns of a session are too short to extract from. They used to
// claim the two-minute throttle slot anyway, so the first turn long enough to
// learn from was skipped too.
func TestShortConversationDoesNotConsumeThrottleSlot(t *testing.T) {
	p := &fakeProvider{response: "NONE"}
	r, _ := newTestRunner(t, p)

	r.Extract(context.Background(), conversation(2))
	r.Extract(context.Background(), conversation(6))

	if got := p.callCount(); got != 1 {
		t.Fatalf("provider called %d times, want 1: the short conversation must not use up the throttle slot", got)
	}
}
