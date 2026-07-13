package api

import (
	"net/http"
	"strconv"
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

// RateLimitTracker manages rate limit state across API calls.
type RateLimitTracker struct {
	mu   sync.RWMutex
	info RateLimitInfo
}

// Info returns the latest parsed rate limit snapshot.
func (t *RateLimitTracker) Info() RateLimitInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.info
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
