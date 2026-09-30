package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// authServer accepts only the key good (as a Bearer token or x-api-key) and
// answers 401 for any other, the way a provider answers a revoked key.
func authServer(t *testing.T, good string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.Header.Get("x-api-key")
		}
		if key != good {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		case r.Header.Get("x-api-key") != "":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pooledOpenAI(srv *httptest.Server, keys ...string) *openAICompatProvider {
	return &openAICompatProvider{keyPool: NewKeyPool(keys), baseURL: srv.URL, client: srv.Client(), streamClient: srv.Client()}
}

var hiRequest = ChatRequest{Model: "deepseek-v4-pro", Messages: []Message{{Role: "user", Content: "hi"}}}

// One revoked key in a pool used to fail the turn: the 401 marked the key
// dead but came back as a non-retryable StatusError, so the next, healthy key
// was never tried, and the fallback chain then blacklisted the provider.
func TestRevokedPoolKeyFailsOverToTheNextKey(t *testing.T) {
	fastRetry(t)
	var calls atomic.Int32
	srv := authServer(t, "good", &calls)

	if resp, err := pooledOpenAI(srv, "bad", "good").Chat(context.Background(), hiRequest); err != nil || resp.Content != "ok" {
		t.Fatalf("Chat with one revoked key: resp=%+v err=%v", resp, err)
	}
	if resp, err := pooledOpenAI(srv, "bad", "good").ChatStream(context.Background(), hiRequest, nil); err != nil || resp.Content != "ok" {
		t.Fatalf("ChatStream with one revoked key: resp=%+v err=%v", resp, err)
	}
	ap := &anthropicProvider{keyPool: NewKeyPool([]string{"bad", "good"}), baseURL: srv.URL, client: srv.Client(), streamClient: srv.Client()}
	if resp, err := ap.Chat(context.Background(), hiRequest); err != nil || resp.Content != "ok" {
		t.Fatalf("anthropic Chat with one revoked key: resp=%+v err=%v", resp, err)
	}
}

// With every key revoked there is nothing to fail over to: the 401 stays a
// permanent auth error, after each key was tried once.
func TestAllPoolKeysRevokedIsPermanent(t *testing.T) {
	fastRetry(t)
	for _, stream := range []bool{false, true} {
		var calls atomic.Int32
		srv := authServer(t, "good", &calls)
		p := pooledOpenAI(srv, "bad1", "bad2")
		var err error
		if stream {
			_, err = p.ChatStream(context.Background(), hiRequest, nil)
		} else {
			_, err = p.Chat(context.Background(), hiRequest)
		}
		if statusOf(err) != http.StatusUnauthorized || !isPermanent(err) {
			t.Fatalf("stream=%v: err = %v, want a permanent 401", stream, err)
		}
		if n := calls.Load(); n != 2 {
			t.Fatalf("stream=%v: %d requests, want one per key (2)", stream, n)
		}
	}
}
