package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// One 503 from the primary moved the chain to the fallback for the rest of
// the session: the primary was retried only when the fallback failed, even
// long after its cooldown had expired.
func TestFallbackReturnsToThePrimaryAfterItsCooldown(t *testing.T) {
	primary := &flakyProvider{errs: []error{&StatusError{Status: 503, Msg: "overloaded"}}}
	backup := &flakyProvider{}
	mf := NewModelFallback([]Provider{primary, backup})
	mf.cooldownDur = time.Hour

	if _, used, err := mf.TryChat(context.Background(), func(Provider) ChatRequest { return ChatRequest{} }); err != nil || used != backup {
		t.Fatalf("first call: used=%v err=%v, want the backup", used, err)
	}
	// Still cooling down: the backup keeps serving.
	if _, used, _ := mf.TryChat(context.Background(), func(Provider) ChatRequest { return ChatRequest{} }); used != backup {
		t.Fatalf("during the primary's cooldown used=%v, want the backup", used)
	}

	mf.mu.Lock()
	mf.providers[0].CoolUntil = time.Now().Add(-time.Second)
	mf.mu.Unlock()
	if _, used, err := mf.TryChat(context.Background(), func(Provider) ChatRequest { return ChatRequest{} }); err != nil || used != primary {
		t.Fatalf("after the cooldown used=%v err=%v, want the primary again", used, err)
	}
}

// A stalled stream is a transient failure, like a timeout: it cools the
// provider down. It used to be unclassified and three of them marked the
// provider unavailable for the session.
func TestStreamStallCoolsDownInsteadOfBlacklisting(t *testing.T) {
	stall := streamStalledError()
	if k := Classify(stall); k != KindTimeout {
		t.Fatalf("Classify(stall) = %v, want timeout", k)
	}
	if !isTemporary(stall) {
		t.Fatal("a stall must be temporary")
	}
	p := &flakyProvider{errs: []error{stall, stall, stall, stall}}
	mf := NewModelFallback([]Provider{p})
	mf.cooldownDur = 0
	for i := 0; i < 4; i++ {
		if err := tryOnce(mf); !errors.Is(err, ErrStreamStalled) {
			t.Fatalf("call %d: err = %v", i, err)
		}
	}
	if st := mf.providers[0].Status; st == ProviderUnavailable {
		t.Fatalf("status after repeated stalls = %v, want degraded, not unavailable", st)
	}
}
