package delegate

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// recordingProvider keeps the requests it was sent and ends the run at once.
type recordingProvider struct{ reqs []api.ChatRequest }

func (p *recordingProvider) Name() string        { return "rec" }
func (p *recordingProvider) DisplayName() string { return "rec" }
func (p *recordingProvider) Validate() error     { return nil }
func (p *recordingProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.reqs = append(p.reqs, req)
	return &api.ChatResponse{Content: "done"}, nil
}
func (p *recordingProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

// Sub-agent requests asked for a fixed 16000 output tokens. deepseek-chat
// allows 8192 and claude-3-haiku 4096, so every sub-agent call on those models
// was rejected with a 400 before doing anything.
func TestSubAgentMaxTokensFollowsTheModel(t *testing.T) {
	for _, model := range []string{"deepseek-chat", "claude-3-haiku-20240307", "claude-sonnet-4-20250514"} {
		prov := &recordingProvider{}
		sa := NewSubAgent(Config{Provider: prov, Model: model})
		sa.Run(context.Background(), "x", "")
		if len(prov.reqs) == 0 {
			t.Fatalf("%s: no request sent", model)
		}
		if got, want := prov.reqs[0].MaxTokens, api.MaxOutputTokensForModel(model); got != want {
			t.Errorf("%s: MaxTokens = %d, want %d", model, got, want)
		}
	}
}
