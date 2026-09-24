package api

import (
	"context"
	"errors"
	"testing"
)

type flakyProvider struct {
	errs  []error // returned in order; nil (or exhausted) means success
	calls int
}

func (f *flakyProvider) Name() string        { return "flaky" }
func (f *flakyProvider) DisplayName() string { return "flaky" }
func (f *flakyProvider) Validate() error     { return nil }
func (f *flakyProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	f.calls++
	if i := f.calls - 1; i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	return &ChatResponse{Content: "ok"}, nil
}
func (f *flakyProvider) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (*ChatResponse, error) {
	return f.Chat(ctx, req)
}

func tryOnce(mf *ModelFallback) error {
	_, _, err := mf.TryChat(context.Background(), func(Provider) ChatRequest { return ChatRequest{} })
	return err
}

// With a single provider there is nothing to fail over to. Cooling it down
// after a transient error made every retry in the next 60 seconds fail without
// even reaching the API.
func TestSingleProviderIsStillTriedDuringCooldown(t *testing.T) {
	p := &flakyProvider{errs: []error{errors.New("read tcp: connection reset by peer")}}
	mf := NewModelFallback([]Provider{p})

	if err := tryOnce(mf); err == nil {
		t.Fatal("first call should fail")
	}
	if err := tryOnce(mf); err != nil {
		t.Fatalf("retry during cooldown failed without reaching the provider: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider called %d times, want 2", p.calls)
	}
}

// Three failures used to blacklist the only provider for the rest of the
// session, even after the user fixed the key.
func TestSingleProviderIsStillTriedAfterBeingMarkedUnavailable(t *testing.T) {
	auth := &StatusError{Status: 401, Msg: "invalid x-api-key"}
	p := &flakyProvider{errs: []error{auth}}
	mf := NewModelFallback([]Provider{p})

	_ = tryOnce(mf)
	if err := tryOnce(mf); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider called %d times, want 2", p.calls)
	}
}

// A request the caller cancelled says nothing about the provider's health.
func TestCallerCancellationDoesNotDegradeTheProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &flakyProvider{errs: []error{context.Canceled}}
	mf := NewModelFallback([]Provider{p})

	_, _, _ = mf.TryChat(ctx, func(Provider) ChatRequest { return ChatRequest{} })
	if st := mf.providers[0].Status; st != ProviderOK {
		t.Fatalf("status after a cancelled call = %v, want OK", st)
	}
	if fc := mf.providers[0].FailCount; fc != 0 {
		t.Fatalf("fail count after a cancelled call = %d, want 0", fc)
	}
}

func TestResetClearsProviderHealth(t *testing.T) {
	p := &flakyProvider{errs: []error{errors.New("i/o timeout")}}
	mf := NewModelFallback([]Provider{p})
	_ = tryOnce(mf)
	mf.Reset()
	if pw := mf.providers[0]; pw.Status != ProviderOK || pw.FailCount != 0 {
		t.Fatalf("after Reset: status=%v fails=%d", pw.Status, pw.FailCount)
	}
}
