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

// ParseRetryAfter reads an integer-seconds Retry-After header. Returns 0 when
// absent or in HTTP-date form (callers fall back to a default cooldown).
func ParseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
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

