package dream

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// MaxTokens was hardcoded to 16000, which a model with a smaller output cap
// (deepseek-chat: 8192) rejects with a 400, so every consolidation failed.
func TestDreamRequestRespectsModelOutputCap(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	writeDreamJSON(t, cfgDir, `{"enabled": true}`)
	touchSession(t, sessions, "s1")
	p := &recordingProvider{}
	_ = RunWorker(context.Background(), p, "deepseek-chat", sessions, "")
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		t.Fatal("the dream run made no request")
	}
	want := api.MaxOutputTokensForModel("deepseek-chat")
	if got := p.reqs[0].MaxTokens; got != want || got > 8192 {
		t.Fatalf("MaxTokens = %d, want %d (the model's output cap)", got, want)
	}
}

// A model with a larger cap still asks for no more than the run used to.
func TestDreamMaxTokensKeepsUpperBound(t *testing.T) {
	if got := dreamMaxTokens("claude-sonnet-4-5"); got > dreamMaxOutputTokens {
		t.Fatalf("dreamMaxTokens = %d, want <= %d", got, dreamMaxOutputTokens)
	}
}
