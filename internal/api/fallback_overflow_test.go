package api

import "testing"

// A request that does not fit the model's window is the request's fault,
// not the provider's: three of them in a row used to mark the only provider
// unavailable and report E2009 instead of the real problem.
func TestContextOverflowDoesNotCountAsProviderFailure(t *testing.T) {
	overflow := &StatusError{Status: 400, Msg: "request (17964 tokens) exceeds the available context size (16384 tokens)"}
	p := &flakyProvider{errs: []error{overflow, overflow, overflow, overflow}}
	mf := NewModelFallback([]Provider{p})
	called := 0
	mf.SetOnUnavailable(func(string, int, error) { called++ })
	for i := 0; i < 4; i++ {
		_ = tryOnce(mf)
	}
	if called != 0 {
		t.Fatalf("overflow marked the provider unavailable %d times", called)
	}
	if st := mf.providers[0].Status; st != ProviderOK {
		t.Fatalf("provider status = %v after overflows, want OK", st)
	}
}
