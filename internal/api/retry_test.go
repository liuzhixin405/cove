package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// Every client that hit the same 429 or 5xx retried on the same schedule
// (1s, 2s, 4s), so they all came back at the same instant and collided again.
func TestRetryDelayIsJittered(t *testing.T) {
	cfg := retryConfig{MaxRetries: 3, BaseDelay: time.Second}
	const attempt = 2
	d := time.Duration(1<<attempt) * cfg.BaseDelay
	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		got := retryDelay(cfg, attempt, 0)
		if got < d/2 || got > d*3/2 {
			t.Fatalf("sample %d: delay %v outside [%v, %v]", i, got, d/2, d*3/2)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatalf("100 delays were all %v: no jitter", retryDelay(cfg, attempt, 0))
	}
}

// Jitter never shortens a wait the server asked for.
func TestRetryDelayNeverUndercutsRetryAfter(t *testing.T) {
	cfg := retryConfig{MaxRetries: 3, BaseDelay: time.Millisecond}
	for i := 0; i < 50; i++ {
		if got := retryDelay(cfg, 0, 3*time.Second); got < 3*time.Second {
			t.Fatalf("delay %v shorter than Retry-After 3s", got)
		}
	}
}

func withNow(t *testing.T, at time.Time) {
	t.Helper()
	old := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = old })
}

// Retry-After may be an HTTP-date (RFC 9110 §10.2.3); that form was read as
// "no wait" and the retry went out immediately into the same limit.
func TestParseRetryAfterHTTPDate(t *testing.T) {
	withNow(t, time.Date(2026, 10, 21, 7, 27, 30, 0, time.UTC))
	h := http.Header{}
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if d := ParseRetryAfter(h); d <= 0 {
		t.Fatalf("HTTP-date Retry-After = %v, want a positive wait", d)
	} else if d != 30*time.Second {
		t.Fatalf("HTTP-date Retry-After = %v, want 30s", d)
	}
	// A date already past asks for no wait.
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:00:00 GMT")
	if d := ParseRetryAfter(h); d != 0 {
		t.Fatalf("past HTTP-date = %v, want 0", d)
	}
}

// OpenAI-style retry-after-ms gives the wait in milliseconds, and wins over
// the coarser Retry-After.
func TestParseRetryAfterMs(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after-ms", "1500")
	if d := ParseRetryAfter(h); d != 1500*time.Millisecond {
		t.Fatalf("retry-after-ms 1500 = %v, want 1.5s", d)
	}
	h.Set("Retry-After", "7")
	if d := ParseRetryAfter(h); d != 1500*time.Millisecond {
		t.Fatalf("with both headers = %v, want the precise 1.5s", d)
	}
}

// Anthropic reports when each limit resets as an RFC 3339 time. On a 429
// without Retry-After the wait is the latest reset among the limits that are
// actually exhausted (remaining 0).
func TestRetryAfterForAnthropicReset(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	withNow(t, now)
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-remaining", "0")
	h.Set("anthropic-ratelimit-requests-reset", now.Add(5*time.Second).Format(time.RFC3339))
	h.Set("anthropic-ratelimit-tokens-remaining", "0")
	h.Set("anthropic-ratelimit-tokens-reset", now.Add(12*time.Second).Format(time.RFC3339))
	h.Set("anthropic-ratelimit-output-tokens-remaining", "9000")
	h.Set("anthropic-ratelimit-output-tokens-reset", now.Add(50*time.Second).Format(time.RFC3339))
	if d := RetryAfterFor(http.StatusTooManyRequests, h); d != 12*time.Second {
		t.Fatalf("429 with reset headers = %v, want 12s (latest exhausted limit; output tokens not exhausted)", d)
	}
	h.Set("Retry-After", "2")
	if d := RetryAfterFor(http.StatusTooManyRequests, h); d != 2*time.Second {
		t.Fatalf("Retry-After present = %v, want it to win (2s)", d)
	}
	// Reset headers ride on every response; only a 429 means a limit hit.
	h.Del("Retry-After")
	if d := RetryAfterFor(http.StatusInternalServerError, h); d != 0 {
		t.Fatalf("500 with reset headers = %v, want 0 (plain backoff)", d)
	}
	if d := ParseRetryAfter(h); d != 0 {
		t.Fatalf("ParseRetryAfter read reset headers: %v", d)
	}
}

// A 500 carrying rate-limit reset headers backs off on the normal schedule,
// not until the (unrelated) limit resets.
func TestServerErrorWithResetHeadersUsesPlainBackoff(t *testing.T) {
	old := defaultRetry
	defaultRetry = retryConfig{MaxRetries: 1, BaseDelay: 100 * time.Millisecond}
	t.Cleanup(func() { defaultRetry = old })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("anthropic-ratelimit-tokens-remaining", "0")
			w.Header().Set("anthropic-ratelimit-tokens-reset", time.Now().Add(30*time.Second).UTC().Format(time.RFC3339))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()
	p := newAnthropicProvider(ProviderConfig{APIKey: "k", BaseURL: server.URL})
	start := time.Now()
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("retry after a 500 waited %v, want <= 2s", el)
	}
}

// A non-streaming request that times out has already had the model generate
// for the whole timeout; retrying repeated that three more times, and the
// user waited up to 20 minutes for one failure.
func TestAnthropicChatTimeoutIsNotRetried(t *testing.T) {
	fastRetry(t)
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	p := newAnthropicProvider(ProviderConfig{APIKey: "k", BaseURL: server.URL})
	if p.client.Timeout != 300*time.Second {
		t.Fatalf("non-streaming timeout = %v, want 300s", p.client.Timeout)
	}
	p.client.Timeout = 100 * time.Millisecond
	_, err := p.Chat(context.Background(), ChatRequest{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1: a timed-out generation was retried", n)
	}
	if isRetryable(err) {
		t.Fatalf("timeout error classified retryable: %v", err)
	}
}

// A connection that fails before anything was sent is still retried.
func TestAnthropicChatConnectionErrorIsRetried(t *testing.T) {
	fastRetry(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("no hijacker")
				return
			}
			c, _, _ := hj.Hijack()
			_ = c.Close() // drop the connection: a transport error, no response
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()
	p := newAnthropicProvider(ProviderConfig{APIKey: "k", BaseURL: server.URL})
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		var re *RetryableError
		t.Fatalf("Chat: %v (retryable=%v)", err, errors.As(err, &re))
	}
	if resp.Content != "ok" || calls.Load() != 2 {
		t.Fatalf("content %q after %d calls, want ok after 2", resp.Content, calls.Load())
	}
}

// isClientTimeout recognises a timeout by type, not by message text.
func TestIsClientTimeoutByType(t *testing.T) {
	if !isClientTimeout(&url.Error{Op: "Post", URL: "http://x", Err: timeoutErr{}}) {
		t.Fatal("url.Error with a timeout not recognised")
	}
	if isClientTimeout(&url.Error{Op: "Post", URL: "http://x", Err: errors.New("Client.Timeout exceeded")}) {
		t.Fatal("non-timeout error recognised by its text")
	}
	if isClientTimeout(errors.New("plain")) {
		t.Fatal("plain error recognised")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "deadline" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
