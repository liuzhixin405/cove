package api

import (
	"testing"
	"time"
)

// Gemini's OpenAI-compatible endpoint names the wait of a 429 only in the
// body.
func TestRetryAfterFromBody(t *testing.T) {
	cases := map[string]time.Duration{
		`{"error":{"code":429,"message":"Quota exceeded ... Please retry in 43.330974807s."}}`:   43330974807,
		`{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay": "12s"}]}`: 12 * time.Second,
		`{"error":"rate limited"}`: 0,
	}
	for body, want := range cases {
		if got := retryAfterFromBody(body); got != want {
			t.Errorf("retryAfterFromBody(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestAnnounceRetry(t *testing.T) {
	var got []string
	SetRetryNotifier(func(s string) { got = append(got, s) })
	defer SetRetryNotifier(nil)
	announceRetry(429, 43*time.Second, 0, 3)
	announceRetry(500, time.Second, 1, 3) // short: not announced
	if len(got) != 1 || got[0] != "请求被限流（429），43 秒后自动重试（第 1/3 次）" {
		t.Fatalf("notices = %q", got)
	}
}
