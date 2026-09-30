package api

import (
	"context"
	"errors"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/token"
)

// UsageFunc receives the usage of one model call: a successful one, or a
// stream that failed after the model had generated (partialUsageError).
// model is the model that actually served the request when the provider
// reports it, and the requested model otherwise.
type UsageFunc func(model string, resp *ChatResponse)

// meteredProvider reports the usage of every call it forwards that the
// provider bills: successful ones and streams that failed mid-generation.
//
// Billing lives here, at the provider, rather than at each call site: the
// engine's main loop used to be the only caller that recorded usage, so
// sub-agents, memory extraction, background review, consolidation and
// compaction summaries all spent tokens the budget never saw.
type meteredProvider struct {
	inner   Provider
	onUsage UsageFunc
}

// NewMeteredProvider wraps inner so every billed call is reported to onUsage.
func NewMeteredProvider(inner Provider, onUsage UsageFunc) Provider {
	return &meteredProvider{inner: inner, onUsage: onUsage}
}

func (m *meteredProvider) Name() string               { return m.inner.Name() }
func (m *meteredProvider) Capabilities() Capabilities { return CapabilitiesOf(m.inner) }
func (m *meteredProvider) DisplayName() string        { return m.inner.DisplayName() }
func (m *meteredProvider) Validate() error            { return m.inner.Validate() }

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
	if m.onUsage == nil {
		return
	}
	if err != nil {
		// A stream that failed after the model generated is billed by the
		// provider all the same; only successful calls used to be reported,
		// so every stall, cut-off stream and in-stream error went past
		// max_budget_usd.
		var pe *partialUsageError
		if !errors.As(err, &pe) {
			return
		}
		resp = pe.usage
	}
	if resp == nil {
		return
	}
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	m.onUsage(model, resp)
}

// partialUsageError is a streamed call that failed after the provider began
// generating, carrying what it will bill for. The usage travels in the
// error, not in a response next to it, so callers that ignore the response
// on error, and wrappers such as the fallback chain that drop it, keep
// working; errors.Is / errors.As see the cause through Unwrap.
type partialUsageError struct {
	err   error
	usage *ChatResponse
}

func (e *partialUsageError) Error() string { return e.err.Error() }
func (e *partialUsageError) Unwrap() error { return e.err }

// withPartialUsage attaches usage to err, a streaming failure after the body
// started. usage holds the counters the stream reported, where an output
// count of 0 means none was final; it is then estimated from generated, the
// text, reasoning and tool arguments streamed so far. With nothing reported
// and nothing generated, err is returned unchanged.
func withPartialUsage(err error, usage *ChatResponse, generated string) error {
	if usage.OutputTokens == 0 {
		usage.OutputTokens = token.Estimate(generated)
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return err
	}
	return &partialUsageError{err: err, usage: usage}
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

func (s *SwitchableProvider) Name() string               { return s.current().Name() }
func (s *SwitchableProvider) Capabilities() Capabilities { return CapabilitiesOf(s.current()) }
func (s *SwitchableProvider) DisplayName() string        { return s.current().DisplayName() }
func (s *SwitchableProvider) Validate() error            { return s.current().Validate() }

func (s *SwitchableProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return s.current().Chat(ctx, req)
}

func (s *SwitchableProvider) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (*ChatResponse, error) {
	return s.current().ChatStream(ctx, req, h)
}
