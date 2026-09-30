package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func shortIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := streamIdleTimeout
	streamIdleTimeout = d
	t.Cleanup(func() { streamIdleTimeout = old })
}

const okOpenAIStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

const okAnthropicStream = "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// The idle watchdog started before the request went out, so the wait for the
// response headers (a local server's prefill, which local.go allows 15
// minutes) counted as idle and ended in a bare context.Canceled — reported
// as if the user had cancelled.
func TestStreamWatchdogIgnoresWaitForHeaders(t *testing.T) {
	shortIdleTimeout(t, 100*time.Millisecond)
	for _, tc := range []struct {
		name string
		body string
		run  func(url string, c *http.Client) (*ChatResponse, error)
	}{
		{"openai", okOpenAIStream, func(url string, c *http.Client) (*ChatResponse, error) {
			p := &openAICompatProvider{apiKey: "k", baseURL: url + "/v1", client: c}
			return p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
		}},
		{"anthropic", okAnthropicStream, func(url string, c *http.Client) (*ChatResponse, error) {
			p := &anthropicProvider{apiKey: "k", baseURL: url + "/v1", client: c}
			return p.ChatStream(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 64, Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(400 * time.Millisecond) // prefill: no headers yet
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			resp, err := tc.run(server.URL, server.Client())
			if err != nil {
				t.Fatalf("ChatStream: %v (Classify=%v)", err, Classify(err))
			}
			if resp.Content != "ok" {
				t.Fatalf("content = %q", resp.Content)
			}
		})
	}
}

// A 429's backoff is part of the connection phase too.
func TestStreamWatchdogIgnoresConnectRetryBackoff(t *testing.T) {
	shortIdleTimeout(t, 100*time.Millisecond)
	old := defaultRetry
	defaultRetry = retryConfig{MaxRetries: 2, BaseDelay: time.Millisecond}
	t.Cleanup(func() { defaultRetry = old })
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After-Ms", "400")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, okOpenAIStream)
	}))
	defer server.Close()
	p := &openAICompatProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
}

// When the body does stall, the error says so and is not a cancellation.
func TestStreamWatchdogStallIsNotACancel(t *testing.T) {
	shortIdleTimeout(t, 150*time.Millisecond)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"o\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	for _, run := range []func() error{
		func() error {
			p := &openAICompatProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}
			_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
			return err
		},
		func() error {
			p := &anthropicProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}
			_, err := p.ChatStream(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 64, Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
			return err
		},
	} {
		err := run()
		if err == nil || !errors.Is(err, ErrStreamStalled) || errors.Is(err, context.Canceled) || Classify(err) == KindCanceled {
			t.Fatalf("err = %v, want ErrStreamStalled and not a cancellation", err)
		}
		if !strings.Contains(err.Error(), "stalled") {
			t.Fatalf("err = %v", err)
		}
	}
}

// retryConnectHTTP rotates keys per attempt and swallows 429/5xx responses,
// but only the final response's key was marked, so a rate-limited key was
// never cooled down and kept being handed out.
func TestOpenStreamMarksEveryAttemptsKey(t *testing.T) {
	old := defaultRetry
	defaultRetry = retryConfig{MaxRetries: 2, BaseDelay: time.Millisecond}
	t.Cleanup(func() { defaultRetry = old })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, okOpenAIStream)
	}))
	defer server.Close()
	pool := NewKeyPool([]string{"k1", "k2"})
	p := &openAICompatProvider{baseURL: server.URL + "/v1", client: server.Client(), keyPool: pool}
	if _, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	pool.mu.Lock()
	st := pool.keys[0].Status
	pool.mu.Unlock()
	if st != KeyExhausted {
		t.Fatalf("k1 status = %v after a 429, want KeyExhausted", st)
	}
	for i := 0; i < 3; i++ {
		if k := pool.Get(); k != "k2" {
			t.Fatalf("Get = %q, want k2 while k1 cools down", k)
		}
	}
}
