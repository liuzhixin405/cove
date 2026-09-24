package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ANTHROPIC_BASE_URL-style values carry no /v1: the Anthropic SDKs and
// the claude CLI append /v1/messages themselves, and DeepSeek documents its
// Anthropic endpoint as https://api.deepseek.com/anthropic. cove appended only
// /messages, so the same setting that works in claude got a 404 here.
func TestAnthropicBaseURLWithoutV1PostsToV1Messages(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	p := newAnthropicProvider(ProviderConfig{APIKey: "k", BaseURL: server.URL + "/anthropic"})
	p.client = server.Client()
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if gotPath != "/anthropic/v1/messages" {
		t.Fatalf("path = %q, want /anthropic/v1/messages", gotPath)
	}
}

func TestAnthropicBaseURLNormalization(t *testing.T) {
	cases := map[string]string{
		"https://api.anthropic.com":             "https://api.anthropic.com/v1",
		"https://api.anthropic.com/":            "https://api.anthropic.com/v1",
		"https://api.anthropic.com/v1":          "https://api.anthropic.com/v1",
		"https://api.anthropic.com/v1/":         "https://api.anthropic.com/v1",
		"https://api.anthropic.com/v1/messages": "https://api.anthropic.com/v1",
		"https://api.deepseek.com/anthropic":    "https://api.deepseek.com/anthropic/v1",
		" https://proxy.example.com/v1 ":        "https://proxy.example.com/v1",
	}
	for in, want := range cases {
		if got := newAnthropicProvider(ProviderConfig{APIKey: "k", BaseURL: in}).baseURL; got != want {
			t.Errorf("base_url %q -> %q, want %q", in, got, want)
		}
	}
}

// Pasting the full endpoint, or a /v1 the default already has, used to
// produce .../chat/completions/chat/completions or /v1/v1 and a 404.
func TestOpenAICompatBaseURLNormalization(t *testing.T) {
	cases := map[string]string{
		"https://api.deepseek.com":                     "https://api.deepseek.com",
		"https://api.deepseek.com/v1/":                 "https://api.deepseek.com/v1",
		"https://api.deepseek.com/v1/chat/completions": "https://api.deepseek.com/v1",
		"https://api.deepseek.com/v1/v1":               "https://api.deepseek.com/v1",
		"https://open.bigmodel.cn/api/paas/v4":         "https://open.bigmodel.cn/api/paas/v4",
		" https://api.deepseek.com/v1 ":                "https://api.deepseek.com/v1",
	}
	for in, want := range cases {
		if got := newOpenAICompatProvider(ProviderConfig{Name: "deepseek", APIKey: "k", BaseURL: in}).baseURL; got != want {
			t.Errorf("base_url %q -> %q, want %q", in, got, want)
		}
	}
}
