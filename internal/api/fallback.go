package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
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
	Model     string // the model used with this provider
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

// CurrentModel returns the model name for the currently active provider.
func (mf *ModelFallback) CurrentModel() string {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	m := mf.providers[mf.currentIdx].Model
	if m == "" {
		m = "unknown"
	}
	return m
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
		mf.mu.Unlock()
		resp, err := call(pw.Provider)
		mf.mu.Lock()

		if err == nil {
			pw.FailCount = 0
			mf.currentIdx = idx
			mf.mu.Unlock()
			return resp, pw.Provider, nil
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
			pw.Status = ProviderUnavailable
			log.Errorf("provider %s marked unavailable after %d failures: %v", pw.Provider.Name(), pw.FailCount, err)
		}

		tried++
	}

	// All providers exhausted
	var msgs []string
	for _, pw := range mf.providers {
		msgs = append(msgs, fmt.Sprintf("%s(%s): %v", pw.Provider.Name(), pw.Status, pw.LastError))
	}
	mf.mu.Unlock()
	return nil, nil, fmt.Errorf("all %d providers failed: %s", len(mf.providers), strings.Join(msgs, "; "))
}

func isRateLimit(err error) bool {
	s := err.Error()
	return strings.Contains(s, "429") ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many requests")
}

func isTemporary(err error) bool {
	s := err.Error()
	return strings.Contains(s, "500") ||
		strings.Contains(s, "502") ||
		strings.Contains(s, "503") ||
		strings.Contains(s, "504") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "temporary")
}

func isPermanent(err error) bool {
	s := err.Error()
	return strings.Contains(s, "401") ||
		strings.Contains(s, "403") ||
		strings.Contains(s, "invalid api key") ||
		strings.Contains(s, "authentication")
}
