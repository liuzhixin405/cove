package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KeyStatus represents the health state of an API key.
type KeyStatus int

const (
	KeyOK        KeyStatus = iota // healthy
	KeyExhausted                  // rate limited, will recover
	KeyDead                       // permanently failed (auth error)
)

// PoolKey is a single API key with its state.
type PoolKey struct {
	Key       string
	Status    KeyStatus
	CoolUntil time.Time
	UseCount  int
	LastError string
}

// KeyPool manages multiple API keys with automatic failover.
type KeyPool struct {
	mu      sync.Mutex
	keys    []*PoolKey
	current int
}

// NewKeyPool creates a pool from a list of API keys.
// If only one key is provided, it still works (no rotation).
func NewKeyPool(keys []string) *KeyPool {
	pool := &KeyPool{
		keys: make([]*PoolKey, 0, len(keys)),
	}
	for _, k := range keys {
		if k != "" {
			pool.keys = append(pool.keys, &PoolKey{Key: k, Status: KeyOK})
		}
	}
	return pool
}

// size returns how many keys the pool holds; 0 for a nil pool.
func (p *KeyPool) size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys)
}

// Get returns the next available API key using round-robin.
// Returns empty string if all keys are exhausted/dead.
func (p *KeyPool) Get() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.keys) == 0 {
		return ""
	}

	now := time.Now()
	// Try starting from current position, wrapping around
	for i := 0; i < len(p.keys); i++ {
		idx := (p.current + i) % len(p.keys)
		k := p.keys[idx]

		// Revive exhausted keys whose cooldown expired
		if k.Status == KeyExhausted && now.After(k.CoolUntil) {
			k.Status = KeyOK
		}

		if k.Status == KeyOK {
			k.UseCount++
			p.current = (idx + 1) % len(p.keys) // advance for next call
			return k.Key
		}
	}

	// All keys unavailable — find the one that recovers soonest
	var soonest *PoolKey
	for _, k := range p.keys {
		if k.Status == KeyExhausted {
			if soonest == nil || k.CoolUntil.Before(soonest.CoolUntil) {
				soonest = k
			}
		}
	}
	if soonest != nil {
		return soonest.Key // use it anyway, API will return 429 and we'll wait
	}
	return ""
}

// MarkRateLimited marks the given key as exhausted with a cooldown.
func (p *KeyPool) MarkRateLimited(key string, cooldown time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.Key == key {
			k.Status = KeyExhausted
			k.CoolUntil = time.Now().Add(cooldown)
			k.LastError = "rate_limited"
			break
		}
	}
}

// MarkDead marks the given key as permanently failed (auth error).
func (p *KeyPool) MarkDead(key string, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.Key == key {
			k.Status = KeyDead
			k.LastError = reason
			break
		}
	}
}

// MarkOutcome updates a key's health from an HTTP status code. This is what
// actually wires the pool's failover: without it every key stays KeyOK forever
// and rotation keeps handing out dead/rate-limited keys.
//
// Safe to call on a nil pool or empty key (no-op), so callers don't need to
// branch on whether multi-key pooling is configured. 5xx and other statuses
// leave the key untouched (not the key's fault).
func (p *KeyPool) MarkOutcome(key string, status int, retryAfter time.Duration) {
	if p == nil || key == "" {
		return
	}
	switch {
	case status == 429:
		if retryAfter <= 0 {
			retryAfter = 60 * time.Second
		}
		p.MarkRateLimited(key, retryAfter)
	case status == 401 || status == 403:
		p.MarkDead(key, fmt.Sprintf("auth error %d", status))
	case status >= 200 && status < 300:
		p.MarkSuccess(key)
	}
}

// ParseRetryAfter returns the wait a response explicitly asks for before the
// next request, or 0 when it names none (callers fall back to a default
// cooldown or the backoff schedule). In order of precedence:
//
//   - retry-after-ms: milliseconds (OpenAI and compatible APIs);
//   - Retry-After: delay-seconds or an HTTP-date (RFC 9110 §10.2.3).
//
// Only the integer-seconds form of Retry-After used to be read; an HTTP-date
// counted as "no wait" and the retry went straight back into the limit.
func ParseRetryAfter(h http.Header) time.Duration {
	if v := strings.TrimSpace(h.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			if secs > 0 {
				return time.Duration(secs) * time.Second
			}
			return 0
		}
		if at, err := http.ParseTime(v); err == nil {
			return untilPositive(at)
		}
	}
	return 0
}

// anthropicLimits are the rate limits Anthropic reports as
// anthropic-ratelimit-<name>-remaining / -reset header pairs.
var anthropicLimits = []string{"requests", "tokens", "input-tokens", "output-tokens"}

// RetryAfterFor is the wait before retrying a response with the given status:
// ParseRetryAfter's explicit headers first; then, for a 429 only, the latest
// anthropic-ratelimit-*-reset (RFC 3339) among the limits whose -remaining
// is 0, since retrying before every exhausted limit resets only fails again.
//
// The reset headers ride on every Anthropic response, 5xx included, and name
// when a window rolls over, not how long to wait; read on a 500 they turned
// a 1s backoff into a wait for an unrelated limit.
func RetryAfterFor(status int, h http.Header) time.Duration {
	if d := ParseRetryAfter(h); d > 0 || status != http.StatusTooManyRequests {
		return d
	}
	var latest time.Duration
	for _, name := range anthropicLimits {
		prefix := "anthropic-ratelimit-" + name
		if strings.TrimSpace(h.Get(prefix+"-remaining")) != "0" {
			continue
		}
		v := strings.TrimSpace(h.Get(prefix + "-reset"))
		if at, err := time.Parse(time.RFC3339, v); err == nil {
			if d := untilPositive(at); d > latest {
				latest = d
			}
		}
	}
	return latest
}

// untilPositive is the time left until at, or 0 once it has passed.
func untilPositive(at time.Time) time.Duration {
	if d := at.Sub(timeNow()); d > 0 {
		return d
	}
	return 0
}

// MarkSuccess resets error state for a key.
func (p *KeyPool) MarkSuccess(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.Key == key {
			k.Status = KeyOK
			k.LastError = ""
			break
		}
	}
}

// timeNow is the clock HTTP-date and reset-time headers are measured against.
// Replaced in tests.
var timeNow = time.Now
