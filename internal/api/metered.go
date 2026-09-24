package api

import (
	"context"
	"sync"
)

// UsageFunc receives the usage of one successful model call. model is the
// model that actually served the request when the provider reports it, and
// the requested model otherwise.
type UsageFunc func(model string, resp *ChatResponse)

// meteredProvider reports the usage of every successful call it forwards.
//
// Billing lives here, at the provider, rather than at each call site: the
// engine's main loop used to be the only caller that recorded usage, so
// sub-agents, memory extraction, background review, consolidation and
// compaction summaries all spent tokens the budget never saw.
type meteredProvider struct {
	inner   Provider
	onUsage UsageFunc
}

// NewMeteredProvider wraps inner so every successful call is reported to onUsage.
func NewMeteredProvider(inner Provider, onUsage UsageFunc) Provider {
	return &meteredProvider{inner: inner, onUsage: onUsage}
}

func (m *meteredProvider) Name() string        { return m.inner.Name() }
func (m *meteredProvider) DisplayName() string { return m.inner.DisplayName() }
func (m *meteredProvider) Validate() error     { return m.inner.Validate() }

func (m *meteredProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	resp, err := m.inner.Chat(ctx, req)
	m.report(req, resp, err)
	return resp, err
}

func (m *meteredProvider) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (*ChatResponse, error) {
	resp, err := m.inner.ChatStream(ctx, req, h)
	m.report(req, resp, err)
	return resp, err
}

func (m *meteredProvider) report(req ChatRequest, resp *ChatResponse, err error) {
	if err != nil || resp == nil || m.onUsage == nil {
		return
	}
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	m.onUsage(model, resp)
}

// SwitchableProvider is a Provider whose target can be replaced at runtime.
//
// Components that are handed a provider once at startup (sub-agents, memory
// extraction, consolidation) hold this instead of the concrete provider, so a
// later /provider or /model switch reaches them too rather than leaving them
// on the old endpoint and key.
type SwitchableProvider struct {
	mu    sync.RWMutex
	inner Provider
}

// NewSwitchableProvider returns a provider that forwards to p until Set.
func NewSwitchableProvider(p Provider) *SwitchableProvider { return &SwitchableProvider{inner: p} }

// Set replaces the provider calls are forwarded to.
func (s *SwitchableProvider) Set(p Provider) {
	s.mu.Lock()
	s.inner = p
	s.mu.Unlock()
}

func (s *SwitchableProvider) current() Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner
}

func (s *SwitchableProvider) Name() string        { return s.current().Name() }
func (s *SwitchableProvider) DisplayName() string { return s.current().DisplayName() }
func (s *SwitchableProvider) Validate() error     { return s.current().Validate() }

func (s *SwitchableProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return s.current().Chat(ctx, req)
}

func (s *SwitchableProvider) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (*ChatResponse, error) {
	return s.current().ChatStream(ctx, req, h)
}
