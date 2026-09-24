package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry shortens the retry backoff for the duration of a test.
func fastRetry(t *testing.T) {
	t.Helper()
	old := defaultRetry
	defaultRetry = retryConfig{MaxRetries: 2, BaseDelay: time.Millisecond}
	t.Cleanup(func() { defaultRetry = old })
}

// A 5xx from the non-streaming path came back as a RetryableError with no
// status, which the fallback classifiers did not see as temporary; three of
// them marked a healthy provider permanently unavailable.
func TestChatServerErrorKeepsItsStatus(t *testing.T) {
	fastRetry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"Service Unavailable"}}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "k", baseURL: server.URL, client: server.Client()}
	_, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-pro", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := statusOf(err); got != http.StatusServiceUnavailable {
		t.Fatalf("statusOf = %d, want 503 (err: %v)", got, err)
	}
	if !isTemporary(err) {
		t.Fatalf("a 503 must be classified temporary: %v", err)
	}
}

// Retry-After tells us when the limit resets; retrying after 1ms just burned
// the remaining attempts on more 429s.
func TestChatHonorsRetryAfterOn429(t *testing.T) {
	fastRetry(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "k", baseURL: server.URL, client: server.Client()}
	start := time.Now()
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-pro", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retried after %v, want to wait the 1s Retry-After", elapsed)
	}
}

func TestChatStreamHonorsRetryAfterOn429(t *testing.T) {
	fastRetry(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "k", baseURL: server.URL, client: server.Client()}
	start := time.Now()
	if _, err := p.ChatStream(context.Background(), ChatRequest{Model: "deepseek-v4-pro", Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retried after %v, want to wait the 1s Retry-After", elapsed)
	}
}

// The engine shows only the first ~120 characters of a failure, and the raw
// JSON body said nothing a user could act on. The status line now leads with
// a Chinese hint for the common failures.
func TestStatusErrorLeadsWithActionableChineseHint(t *testing.T) {
	cases := map[int]string{
		401: "API Key",
		402: "余额不足",
		403: "权限",
		413: "请求过大",
		429: "限流",
		503: "服务",
	}
	for status, want := range cases {
		msg := (&StatusError{Status: status, Msg: `{"error":{"message":"whatever"}}`}).Error()
		head := []rune(msg)
		if len(head) > 60 {
			head = head[:60]
		}
		if !strings.Contains(string(head), want) {
			t.Errorf("status %d: %q should start with a hint containing %q", status, msg, want)
		}
	}
}

func TestStatusErrorExplainsContextOverflow(t *testing.T) {
	err := &StatusError{Status: 400, Msg: `{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 140000 tokens","type":"invalid_request_error"}}`}
	if !strings.Contains(err.Error(), "上下文") {
		t.Fatalf("error = %q, want a context-length hint", err.Error())
	}
}

// Context overflow needs its own recovery (compact and retry), so it has to
// be recognizable through the fallback wrapper, across provider wordings.
func TestIsContextLengthError(t *testing.T) {
	yes := []error{
		&StatusError{Status: 400, Msg: `This model's maximum context length is 65536 tokens. However, you requested 70000 tokens`},
		&StatusError{Status: 400, Msg: `{"error":{"code":"context_length_exceeded"}}`},
		&StatusError{Status: 400, Msg: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`},
		&StatusError{Status: 413, Msg: `{"type":"error","error":{"type":"request_too_large"}}`},
		fmt.Errorf("api: %w", &StatusError{Status: 400, Msg: "input length exceeds the context window"}),
	}
	for _, err := range yes {
		if !IsContextLengthError(err) {
			t.Errorf("IsContextLengthError(%v) = false, want true", err)
		}
	}
	no := []error{
		&StatusError{Status: 400, Msg: `max_tokens must be at most 8192`},
		&StatusError{Status: 401, Msg: `invalid api key`},
		errors.New("connection reset by peer"),
		nil,
	}
	for _, err := range no {
		if IsContextLengthError(err) {
			t.Errorf("IsContextLengthError(%v) = true, want false", err)
		}
	}
}

// With one provider there is nothing to fail over to, and the old
// "all 1 providers failed: name(status): ..." prefix pushed the real reason
// past what the UI shows. It also flattened the error with %v, so the engine
// could not recognise its status (errors.As) at all.
func TestSingleProviderFailureKeepsTheProviderError(t *testing.T) {
	cause := &StatusError{Status: 402, Msg: "Insufficient Balance"}
	mf := NewModelFallback([]Provider{&flakyProvider{errs: []error{cause, cause}}})
	err := tryOnce(mf)
	if statusOf(err) != 402 {
		t.Fatalf("statusOf(%v) = %d, want 402", err, statusOf(err))
	}
	if strings.Contains(err.Error(), "providers failed") {
		t.Fatalf("error = %q, want the provider's own error", err)
	}
}

func TestMultiProviderFailureStillUnwrapsToCauses(t *testing.T) {
	a := &flakyProvider{errs: []error{&StatusError{Status: 400, Msg: "prompt is too long: 300000 tokens > 200000 maximum"}}}
	b := &flakyProvider{errs: []error{&StatusError{Status: 400, Msg: "maximum context length is 131072 tokens"}}}
	err := tryOnce(NewModelFallback([]Provider{a, b}))
	if err == nil || !strings.Contains(err.Error(), "providers failed") {
		t.Fatalf("error = %v, want the combined failure", err)
	}
	if !IsContextLengthError(err) {
		t.Fatalf("IsContextLengthError(%v) = false through the fallback wrapper", err)
	}
}
