package api

import (
	"strconv"
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

// Size returns total key count.
func (p *KeyPool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys)
}

// Available returns count of currently usable keys.
func (p *KeyPool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	count := 0
	for _, k := range p.keys {
		if k.Status == KeyOK || (k.Status == KeyExhausted && now.After(k.CoolUntil)) {
			count++
		}
	}
	return count
}

// Status returns a summary for display.
func (p *KeyPool) Status() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) <= 1 {
		return ""
	}
	ok, exhausted, dead := 0, 0, 0
	now := time.Now()
	for _, k := range p.keys {
		switch {
		case k.Status == KeyDead:
			dead++
		case k.Status == KeyExhausted && now.Before(k.CoolUntil):
			exhausted++
		default:
			ok++
		}
	}
	return "Keys: " + strconv.Itoa(ok) + "可用/" + strconv.Itoa(exhausted) + "冷却/" + strconv.Itoa(dead) + "失效"
}
