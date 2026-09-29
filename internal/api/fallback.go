package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/log"
)

// ProviderStatus represents the health state of a provider.
type ProviderStatus int

const (
	ProviderOK          ProviderStatus = iota // healthy, ready to use
	ProviderDegraded                          // rate-limited / temporary error, cooling down
	ProviderUnavailable                       // permanent failure, needs manual intervention
)

func (s ProviderStatus) String() string {
	switch s {
	case ProviderOK:
		return "●"
	case ProviderDegraded:
		return "○"
	case ProviderUnavailable:
		return "✕"
	default:
		return "?"
	}
}

// ProviderWithStatus wraps a Provider with health tracking.
type ProviderWithStatus struct {
	Provider  Provider
	Status    ProviderStatus
	CoolUntil time.Time
	FailCount int
	LastError error
}

// ProviderStatusInfo is a public snapshot for UI display.
type ProviderStatusInfo struct {
	Name   string
	Model  string
	Status ProviderStatus
}

// ModelFallback manages a chain of providers with automatic failover.
// When the primary provider fails (rate limit, timeout, 5xx), it automatically
// switches to the next available provider. Degraded providers cool down for
// a configurable duration before retry. Permanently unavailable providers
// are skipped until manually restored.
type ModelFallback struct {
	mu          sync.Mutex
	providers   []*ProviderWithStatus
	currentIdx  int
	cooldownDur time.Duration
	maxFails    int
	// onUnavailable, when set, is told once each time a provider is marked
	// unavailable (SetOnUnavailable); the engine routes it to diagnostics.
	onUnavailable func(provider string, fails int, cause error)
}

// SetOnUnavailable registers the callback told when a provider is marked
// unavailable after repeated failures. It runs after the chain's lock is
// released, on the calling goroutine, so it may print and take other locks.
func (mf *ModelFallback) SetOnUnavailable(fn func(provider string, fails int, cause error)) {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	mf.onUnavailable = fn
}

// NewModelFallback creates a fallback chain from a list of providers.
// At least one provider is required.
func NewModelFallback(providers []Provider) *ModelFallback {
	if len(providers) == 0 {
		panic("ModelFallback requires at least one provider")
	}
	mf := &ModelFallback{
		cooldownDur: 60 * time.Second,
		maxFails:    3,
	}
	for _, p := range providers {
		mf.providers = append(mf.providers, &ProviderWithStatus{
			Provider: p,
			Status:   ProviderOK,
		})
	}
	return mf
}

// Current returns the currently active provider (without trying it).
func (mf *ModelFallback) Current() Provider {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	return mf.providers[mf.currentIdx].Provider
}

// TryChat attempts a chat request with automatic failover.
// On success, returns the response and the provider used.
// On failure after all providers are exhausted, returns an error.
func (mf *ModelFallback) TryChat(
	ctx context.Context,
	buildRequest func(Provider) ChatRequest,
) (*ChatResponse, Provider, error) {
	return mf.try(ctx, func(p Provider) (*ChatResponse, error) {
		return p.Chat(ctx, buildRequest(p))
	})
}

// TryChatStream is like TryChat but for streaming requests.
func (mf *ModelFallback) TryChatStream(
	ctx context.Context,
	buildRequest func(Provider) ChatRequest,
	handler StreamHandler,
) (*ChatResponse, Provider, error) {
	return mf.try(ctx, func(p Provider) (*ChatResponse, error) {
		return p.ChatStream(ctx, buildRequest(p), handler)
	})
}

func (mf *ModelFallback) try(
	ctx context.Context,
	call func(Provider) (*ChatResponse, error),
) (*ChatResponse, Provider, error) {
	mf.mu.Lock()
	startIdx := mf.currentIdx
	tried := 0
	called := false
	// unavailable is the onUnavailable call to make once the lock is
	// released: the callback prints and takes other locks, so it must not
	// run under mf.mu.
	var unavailable func()
	fire := func() {
		if unavailable != nil {
			unavailable()
			unavailable = nil
		}
	}

	for tried < len(mf.providers) {
		idx := (startIdx + tried) % len(mf.providers)
		pw := mf.providers[idx]

		// Check availability (under lock)
		switch pw.Status {
		case ProviderUnavailable:
			tried++
			continue
		case ProviderDegraded:
			if time.Now().Before(pw.CoolUntil) {
				tried++
				continue
			}
			pw.Status = ProviderOK
			log.Debugf("provider %s cooldown expired, restored", pw.Provider.Name())
		}

		// Release lock during the actual API call to avoid blocking status reads
		called = true
		mf.mu.Unlock()
		fire()
		resp, err := call(pw.Provider)
		mf.mu.Lock()

		if err == nil {
			pw.FailCount = 0
			mf.currentIdx = idx
			mf.mu.Unlock()
			fire()
			return resp, pw.Provider, nil
		}

		// A request the caller cancelled (Ctrl+C, turn timeout) says nothing
		// about the provider's health; counting it would cool down or
		// blacklist a working provider.
		if ctx.Err() != nil {
			mf.mu.Unlock()
			fire()
			return nil, nil, err
		}

		// A request that does not fit the model's window is the request's
		// fault, not the provider's: counted, three of them marked the only
		// provider unavailable and reported that instead of the overflow.
		if IsContextLengthError(err) {
			pw.LastError = err
			tried++
			continue
		}

		// Handle failure (under lock)
		pw.FailCount++
		pw.LastError = err

		if isRateLimit(err) {
			pw.Status = ProviderDegraded
			pw.CoolUntil = time.Now().Add(mf.cooldownDur)
			log.Warnf("provider %s rate-limited, cooling until %s", pw.Provider.Name(), pw.CoolUntil.Format(time.RFC3339))
		} else if isTemporary(err) {
			pw.Status = ProviderDegraded
			pw.CoolUntil = time.Now().Add(mf.cooldownDur)
			log.Warnf("provider %s temporary error, cooling: %v", pw.Provider.Name(), err)
		} else if pw.FailCount >= mf.maxFails || isPermanent(err) {
			wasUnavailable := pw.Status == ProviderUnavailable
			pw.Status = ProviderUnavailable
			// Debug, not Error: the diagnostic layer records this through
			// onUnavailable with a code and a hint; an Errorf would land in
			// the same log a second time, uncoded, via the log sink.
			log.Debugf("provider %s marked unavailable after %d failures: %v", pw.Provider.Name(), pw.FailCount, err)
			if fn := mf.onUnavailable; fn != nil && !wasUnavailable {
				name, fails, cause := pw.Provider.Name(), pw.FailCount, err
				unavailable = func() { fn(name, fails, cause) }
			}
		}

		tried++
	}

	// Every provider was skipped as cooling down or unavailable, so nothing
	// was actually attempted. Skipping only makes sense when there is somewhere
	// else to go; with nowhere left, refusing to call anything turned one
	// transient error into a 60-second outage (and three into a permanently
	// dead session). Try the preferred provider anyway.
	if !called {
		pw := mf.providers[startIdx]
		mf.mu.Unlock()
		fire()
		resp, err := call(pw.Provider)
		mf.mu.Lock()
		if err == nil {
			pw.Status = ProviderOK
			pw.FailCount = 0
			mf.mu.Unlock()
			return resp, pw.Provider, nil
		}
		if ctx.Err() == nil {
			pw.LastError = err
		}
	}

	// All providers exhausted. With a single provider its own error is the
	// whole story: the "all 1 providers failed: name(status):" prefix used to
	// push the actual reason past what the UI shows.
	if len(mf.providers) == 1 && mf.providers[0].LastError != nil {
		err := mf.providers[0].LastError
		mf.mu.Unlock()
		fire()
		return nil, nil, err
	}
	var msgs []string
	var causes []error
	for _, pw := range mf.providers {
		msgs = append(msgs, fmt.Sprintf("%s(%s): %v", pw.Provider.Name(), pw.Status, pw.LastError))
		if pw.LastError != nil {
			causes = append(causes, pw.LastError)
		}
	}
	mf.mu.Unlock()
	fire()
	return nil, nil, &allProvidersError{
		msg:    fmt.Sprintf("all %d providers failed: %s", len(mf.providers), strings.Join(msgs, "; ")),
		causes: causes,
	}
}

// allProvidersError keeps every provider's error reachable through
// errors.As / IsContextLengthError; the old fmt.Errorf("%v") flattened them
// into text, so callers could no longer tell a context overflow or a 402
// from anything else.
type allProvidersError struct {
	msg    string
	causes []error
}

func (e *allProvidersError) Error() string   { return e.msg }
func (e *allProvidersError) Unwrap() []error { return e.causes }

// Reset clears every provider's health so a freshly switched provider starts
// from a clean slate.
func (mf *ModelFallback) Reset() {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	for _, pw := range mf.providers {
		pw.Status = ProviderOK
		pw.FailCount = 0
		pw.CoolUntil = time.Time{}
		pw.LastError = nil
	}
}
