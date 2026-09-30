package api

import (
	"net/http"
	"testing"
	"time"
)

// Anthropic's anthropic-ratelimit-*-reset headers are RFC 3339 timestamps
// ("2026-09-25T12:00:12Z"). headerDuration only knew Go durations ("1m30s")
// and bare seconds, so every Anthropic reset parsed as 0 and the status line
// never showed when the limit would clear.
func TestRateLimitTrackerParsesRFC3339Reset(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	withNow(t, now)

	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "50")
	h.Set("anthropic-ratelimit-requests-remaining", "49")
	h.Set("anthropic-ratelimit-requests-reset", now.Add(12*time.Second).Format(time.RFC3339))
	h.Set("anthropic-ratelimit-tokens-limit", "40000")
	h.Set("anthropic-ratelimit-tokens-remaining", "39000")
	h.Set("anthropic-ratelimit-tokens-reset", now.Add(90*time.Second).Format(time.RFC3339Nano))

	tr := NewRateLimitTracker()
	tr.Update(h)
	info := tr.Info()
	if info.RequestsLimit != 50 || info.TokensLimit != 40000 {
		t.Fatalf("limits not parsed: %+v", info)
	}
	if info.RequestsReset != 12*time.Second {
		t.Fatalf("RequestsReset = %v, want 12s", info.RequestsReset)
	}
	if info.TokensReset != 90*time.Second {
		t.Fatalf("TokensReset = %v, want 90s", info.TokensReset)
	}
}

// A reset time already in the past is "now", never a negative wait.
func TestRateLimitTrackerClampsPastResetToZero(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	withNow(t, now)
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "50")
	h.Set("anthropic-ratelimit-requests-reset", now.Add(-time.Minute).Format(time.RFC3339))
	tr := NewRateLimitTracker()
	tr.Update(h)
	if d := tr.Info().RequestsReset; d != 0 {
		t.Fatalf("RequestsReset = %v, want 0 for a reset in the past", d)
	}
}

// The OpenAI forms ("1m30s", "6s", "0.5") still work.
func TestHeaderDurationKeepsDurationAndSecondsForms(t *testing.T) {
	cases := map[string]time.Duration{
		"1m30s": 90 * time.Second,
		"6s":    6 * time.Second,
		"2.5":   2500 * time.Millisecond,
		"":      0,
		"junk":  0,
	}
	for v, want := range cases {
		h := http.Header{}
		h.Set("x-ratelimit-reset-requests", v)
		if got := headerDuration(h, "x-ratelimit-reset-requests"); got != want {
			t.Errorf("headerDuration(%q) = %v, want %v", v, got, want)
		}
	}
}
