package api

import (
	"context"
	"errors"
	"testing"
)

type usageStub struct {
	name  string
	resp  *ChatResponse
	err   error
	calls int
}

func (s *usageStub) Name() string        { return s.name }
func (s *usageStub) DisplayName() string { return s.name + "-display" }
func (s *usageStub) Validate() error     { return nil }
func (s *usageStub) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	s.calls++
	return s.resp, s.err
}
func (s *usageStub) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (*ChatResponse, error) {
	s.calls++
	return s.resp, s.err
}

type usageRecord struct {
	model   string
	in, out int
}

// Every model call has to be billed, whoever makes it. The meter sits on the
// provider so callers cannot forget to report usage.
func TestMeteredProviderBillsChatAndStream(t *testing.T) {
	inner := &usageStub{name: "anthropic", resp: &ChatResponse{InputTokens: 100, OutputTokens: 7}}
	var got []usageRecord
	p := NewMeteredProvider(inner, func(model string, resp *ChatResponse) {
		got = append(got, usageRecord{model, resp.InputTokens, resp.OutputTokens})
	})

	if _, err := p.Chat(context.Background(), ChatRequest{Model: "req-model"}); err != nil {
		t.Fatal(err)
	}
	inner.resp = &ChatResponse{Model: "served-model", InputTokens: 3, OutputTokens: 4}
	if _, err := p.ChatStream(context.Background(), ChatRequest{Model: "req-model"}, nil); err != nil {
		t.Fatal(err)
	}

	want := []usageRecord{{"req-model", 100, 7}, {"served-model", 3, 4}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("billed %v, want %v (served model wins over requested)", got, want)
	}
	if p.Name() != "anthropic" || p.DisplayName() != "anthropic-display" {
		t.Fatalf("identity not forwarded: %s / %s", p.Name(), p.DisplayName())
	}
}

func TestMeteredProviderDoesNotBillFailedCalls(t *testing.T) {
	inner := &usageStub{name: "x", err: errors.New("boom")}
	billed := 0
	p := NewMeteredProvider(inner, func(string, *ChatResponse) { billed++ })
	_, _ = p.Chat(context.Background(), ChatRequest{Model: "m"})
	_, _ = p.ChatStream(context.Background(), ChatRequest{Model: "m"}, nil)
	if billed != 0 {
		t.Fatalf("billed %d failed calls", billed)
	}
}

// Components that were handed a provider at startup must follow a later
// provider switch instead of keeping the stale one.
func TestSwitchableProviderFollowsLatestProvider(t *testing.T) {
	first := &usageStub{name: "first", resp: &ChatResponse{}}
	second := &usageStub{name: "second", resp: &ChatResponse{}}
	sw := NewSwitchableProvider(first)
	holder := Provider(sw) // what a component would have captured at startup

	sw.Set(second)
	if _, err := holder.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if first.calls != 0 || second.calls != 1 {
		t.Fatalf("calls first=%d second=%d, want the switched-to provider to serve", first.calls, second.calls)
	}
	if holder.Name() != "second" {
		t.Fatalf("Name() = %q after switch", holder.Name())
	}
}

// The OpenAI-compatible side has the same concern: consecutive user turns (a
// real message followed by engine context) are sent as one message, since
// some compatible backends reject successive user messages.
func TestOpenAICompatMergesConsecutiveUserMessages(t *testing.T) {
	p := &openAICompatProvider{}
	out := p.convertMessages([]Message{
		{Role: "user", Content: "fix the bug"},
		{Role: "user", Content: "<env>git: clean</env>", Synthetic: true},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Input: map[string]any{}}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "user", Content: "[system: next]", Synthetic: true},
	})
	if len(out) != 4 {
		t.Fatalf("got %d messages, want 4 (the two leading user turns merged)", len(out))
	}
	if out[0].Content != "fix the bug\n\n<env>git: clean</env>" {
		t.Fatalf("merged content = %#v", out[0].Content)
	}
	if out[2].Role != "tool" || out[3].Role != "user" {
		t.Fatalf("a user turn after a tool result must stay separate, got roles %s,%s", out[2].Role, out[3].Role)
	}
}
