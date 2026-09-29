package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// unavailableEvent records what the chain's callback received, so the test
// can assert the count, the failure count and the tipping error without the
// ProviderUnavailableError type the engine used to report (that type is gone:
// the chain no longer runs in production, and the diagnostic layer reports
// the provider's own error directly).
type unavailableEvent struct {
	provider string
	fails    int
	cause    error
}

// When the chain marks a provider unavailable it tells the registered
// callback once, with the failure count and the error that tipped it, so the
// diagnostic layer can record E2009 with its cause.
func TestFallbackReportsProviderUnavailableOnce(t *testing.T) {
	cause := &StatusError{Status: 400, Msg: `{"error":{"message":"model qwen-x does not exist"}}`}
	p := &flakyProvider{errs: []error{cause, cause, cause, cause}}
	mf := NewModelFallback([]Provider{p})
	var got []unavailableEvent
	mf.SetOnUnavailable(func(provider string, fails int, err error) {
		got = append(got, unavailableEvent{provider: provider, fails: fails, cause: err})
	})
	for i := 0; i < 4; i++ {
		_, _, _ = mf.TryChat(context.Background(), func(Provider) ChatRequest { return ChatRequest{} })
	}
	if len(got) != 1 {
		t.Fatalf("callback called %d times, want once", len(got))
	}
	if got[0].fails != 3 || !errors.Is(got[0].cause, cause) {
		t.Errorf("event = %+v", got[0])
	}
}

// The callback runs after the chain's lock is released, so it may call back
// into the chain (the diagnostic layer behind it prints and takes other
// locks) without deadlocking.
func TestUnavailableCallbackRunsOutsideTheChainLock(t *testing.T) {
	cause := &StatusError{Status: 401, Msg: "bad key"}
	mf := NewModelFallback([]Provider{&flakyProvider{errs: []error{cause}}})
	done := make(chan struct{})
	mf.SetOnUnavailable(func(string, int, error) {
		_ = mf.Current() // would deadlock under mf.mu
		close(done)
	})
	go func() { _ = tryOnce(mf) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callback blocked: it runs under the chain's lock")
	}
}
