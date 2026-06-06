package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitInfo holds parsed rate limit data from API response headers.
type RateLimitInfo struct {
	RequestsLimit     int
	RequestsRemaining int
	RequestsReset     time.Duration
	TokensLimit       int
	TokensRemaining   int
	TokensReset       time.Duration
	UpdatedAt         time.Time
}

// HasData returns true if any rate limit info was parsed.
func (r *RateLimitInfo) HasData() bool {
	return r.RequestsLimit > 0 || r.TokensLimit > 0
}

// Format returns a human-readable rate limit summary.
func (r *RateLimitInfo) Format() string {
	if !r.HasData() {
		return ""
	}
	var parts []string
	if r.RequestsLimit > 0 {
		parts = append(parts, formatBucket("请求", r.RequestsRemaining, r.RequestsLimit, r.RequestsReset))
	}
	if r.TokensLimit > 0 {
		parts = append(parts, formatBucket("Token", r.TokensRemaining, r.TokensLimit, r.TokensReset))
	}
	return strings.Join(parts, " | ")
}

func formatBucket(name string, remaining, limit int, reset time.Duration) string {
	pct := 0
	if limit > 0 {
		pct = remaining * 100 / limit
	}
	remainStr := formatCount(remaining)
	limitStr := formatCount(limit)
	resetStr := ""
	if reset > 0 {
		resetStr = " 重置:" + formatDuration(reset)
	}
	return name + ":" + remainStr + "/" + limitStr + "(" + strconv.Itoa(pct) + "%)" + resetStr
}

func formatCount(n int) string {
	if n >= 1_000_000 {
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	}
	if n >= 1_000 {
		return strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64) + "K"
	}
	return strconv.Itoa(n)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	if s == 0 {
		return strconv.Itoa(m) + "m"
	}
	return strconv.Itoa(m) + "m" + strconv.Itoa(s) + "s"
}

// RateLimitTracker manages rate limit state across API calls.
type RateLimitTracker struct {
	mu   sync.RWMutex
	info RateLimitInfo
}

// NewRateLimitTracker creates a new rate limit tracker.
func NewRateLimitTracker() *RateLimitTracker {
	return &RateLimitTracker{}
}

// Update parses rate limit headers from an HTTP response.
func (t *RateLimitTracker) Update(headers http.Header) {
	info := RateLimitInfo{UpdatedAt: time.Now()}

	info.RequestsLimit = headerInt(headers, "x-ratelimit-limit-requests")
	info.RequestsRemaining = headerInt(headers, "x-ratelimit-remaining-requests")
	info.RequestsReset = headerDuration(headers, "x-ratelimit-reset-requests")
	info.TokensLimit = headerInt(headers, "x-ratelimit-limit-tokens")
	info.TokensRemaining = headerInt(headers, "x-ratelimit-remaining-tokens")
	info.TokensReset = headerDuration(headers, "x-ratelimit-reset-tokens")

	// Also check anthropic-specific headers
	if info.RequestsLimit == 0 {
		info.RequestsLimit = headerInt(headers, "anthropic-ratelimit-requests-limit")
		info.RequestsRemaining = headerInt(headers, "anthropic-ratelimit-requests-remaining")
		info.RequestsReset = headerDuration(headers, "anthropic-ratelimit-requests-reset")
	}
	if info.TokensLimit == 0 {
		info.TokensLimit = headerInt(headers, "anthropic-ratelimit-tokens-limit")
		info.TokensRemaining = headerInt(headers, "anthropic-ratelimit-tokens-remaining")
		info.TokensReset = headerDuration(headers, "anthropic-ratelimit-tokens-reset")
	}

	if info.HasData() {
		t.mu.Lock()
		t.info = info
		t.mu.Unlock()
	}
}

// Current returns the latest rate limit info.
func (t *RateLimitTracker) Current() RateLimitInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.info
}

func headerInt(h http.Header, key string) int {
	v := h.Get(key)
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func headerDuration(h http.Header, key string) time.Duration {
	v := h.Get(key)
	if v == "" {
		return 0
	}
	// Parse formats like "1m30s", "45s", "2m"
	d, err := time.ParseDuration(v)
	if err == nil {
		return d
	}
	// Try parsing as seconds
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}
