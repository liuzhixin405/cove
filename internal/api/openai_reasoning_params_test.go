package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureOpenAIRequest runs Chat (or ChatStream) for model against a server
// that records the JSON body and returns the decoded body.
func captureOpenAIRequest(t *testing.T, model string, stream bool) map[string]any {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`)
	}))
	t.Cleanup(server.Close)

	p := &openAICompatProvider{name: "openai", apiKey: "k", baseURL: server.URL, client: server.Client()}
	req := ChatRequest{Model: model, MaxTokens: 1234, Messages: []Message{{Role: "user", Content: "hi"}}}
	var err error
	if stream {
		_, err = p.ChatStream(context.Background(), req, nil)
	} else {
		_, err = p.Chat(context.Background(), req)
	}
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return body
}

// OpenAI's reasoning models (o1, o3, o4, gpt-5) reject max_tokens
// ("Unsupported parameter: 'max_tokens' ... Use 'max_completion_tokens'").
// The request used to send max_tokens for every model, so those all failed
// with a 400. They get max_completion_tokens and no max_tokens.
func TestReasoningModelsSendMaxCompletionTokens(t *testing.T) {
	for _, model := range []string{"o1", "o1-mini", "o3", "o3-mini", "o4-mini", "gpt-5", "gpt-5-mini", "gpt-5.1", "openai/o3-mini"} {
		for _, stream := range []bool{false, true} {
			body := captureOpenAIRequest(t, model, stream)
			if _, has := body["max_tokens"]; has {
				t.Errorf("%s (stream=%v): request still carries max_tokens, which OpenAI rejects", model, stream)
			}
			if got, _ := body["max_completion_tokens"].(float64); got != 1234 {
				t.Errorf("%s (stream=%v): max_completion_tokens = %v, want 1234", model, stream, body["max_completion_tokens"])
			}
			if _, has := body["temperature"]; has {
				t.Errorf("%s (stream=%v): reasoning models reject a temperature, none must be sent", model, stream)
			}
		}
	}
}

// Everything else keeps max_tokens: third-party compatible servers (DeepSeek,
// llama.cpp, vLLM) do not all understand max_completion_tokens.
func TestNonReasoningModelsKeepMaxTokens(t *testing.T) {
	for _, model := range []string{"gpt-4o", "gpt-4o-mini", "deepseek-v4-pro", "deepseek-reasoner", "qwen-plus", "gpt-4.1"} {
		body := captureOpenAIRequest(t, model, false)
		if got, _ := body["max_tokens"].(float64); got != 1234 {
			t.Errorf("%s: max_tokens = %v, want 1234", model, body["max_tokens"])
		}
		if _, has := body["max_completion_tokens"]; has {
			t.Errorf("%s: must not send max_completion_tokens", model)
		}
	}
}

func TestIsOpenAIReasoningModel(t *testing.T) {
	yes := []string{"o1", "o1-mini", "o1-preview", "o3", "o3-mini", "o3-pro", "o4-mini", "gpt-5", "gpt-5-nano", "gpt-5.1", "GPT-5-Mini", "openai/o4-mini", "azure/gpt-5"}
	no := []string{"gpt-4o", "gpt-4.1", "gpt-4o-mini", "deepseek-reasoner", "deepseek-r1", "qwen3", "o10-mini", "moonshot-o3", "claude-opus-5"}
	for _, m := range yes {
		if !isOpenAIReasoningModel(m) {
			t.Errorf("isOpenAIReasoningModel(%q) = false, want true", m)
		}
	}
	for _, m := range no {
		if isOpenAIReasoningModel(m) {
			t.Errorf("isOpenAIReasoningModel(%q) = true, want false", m)
		}
	}
}
