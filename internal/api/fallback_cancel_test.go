package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Every provider is cooling down, so the chain forces a call to the primary
// anyway. When that call fails because the caller cancelled, the error used
// to be dropped (a cancellation is not the provider's fault) and the chain
// fell through to reporting the provider's stale LastError — the 503 from a
// minute ago — as the reason this Ctrl+C'd request failed. The ctx error is
// the answer.
func TestForcedPrimaryCallReturnsContextErrorNotStaleLastError(t *testing.T) {
	stale := &StatusError{Status: 503, Msg: "overloaded a minute ago"}
	p := &flakyProvider{errs: []error{context.Canceled}}
	mf := NewModelFallback([]Provider{p})
	mf.mu.Lock()
	mf.providers[0].Status = ProviderDegraded
	mf.providers[0].CoolUntil = time.Now().Add(time.Hour)
	mf.providers[0].LastError = stale
	mf.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := mf.TryChat(ctx, func(Provider) ChatRequest { return ChatRequest{} })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if p.calls != 1 {
		t.Fatalf("provider called %d times, want 1 (the forced primary call)", p.calls)
	}
	// The cancellation must not be recorded against the provider's health.
	mf.mu.Lock()
	defer mf.mu.Unlock()
	if mf.providers[0].LastError != stale {
		t.Fatalf("LastError = %v, want the stale error left untouched", mf.providers[0].LastError)
	}
}

// Same with two providers, both cooling: the combined "all providers failed"
// error must not replace a cancellation either.
func TestForcedPrimaryCallWithSeveralCoolingProvidersReturnsContextError(t *testing.T) {
	a := &flakyProvider{errs: []error{context.Canceled}}
	b := &flakyProvider{}
	mf := NewModelFallback([]Provider{a, b})
	mf.mu.Lock()
	for _, pw := range mf.providers {
		pw.Status = ProviderDegraded
		pw.CoolUntil = time.Now().Add(time.Hour)
		pw.LastError = &StatusError{Status: 429, Msg: "stale"}
	}
	mf.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := mf.TryChat(ctx, func(Provider) ChatRequest { return ChatRequest{} })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if b.calls != 0 {
		t.Fatalf("the backup was called %d times during its cooldown", b.calls)
	}
}
