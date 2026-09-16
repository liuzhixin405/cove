package dream

import (
	"context"
	"testing"
	"time"
)

// TestDeriveDreamContext_DetachedFromParent locks in the C-1 fix: the background
// consolidation context must NOT be cancelled just because the caller (the turn
// that triggered the dream) cancels its own context. With the former
// context.WithCancel(parent) wiring this failed immediately.
func TestDeriveDreamContext_DetachedFromParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := deriveDreamContext(parent)
	defer cancel()

	cancelParent() // caller's turn ends and cancels its context

	select {
	case <-ctx.Done():
		t.Fatal("dream context was cancelled when the caller cancelled its context (C-1): background consolidation would abort immediately")
	case <-time.After(50 * time.Millisecond):
		// still alive — correct
	}
}

// TestDeriveDreamContext_HasTimeout verifies the detached context still carries a
// deadline, so a stuck run cannot leak forever.
func TestDeriveDreamContext_HasTimeout(t *testing.T) {
	ctx, cancel := deriveDreamContext(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("dream context has no deadline; a stuck run could leak")
	}
}
