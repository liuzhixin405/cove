package api

import (
	"net/http"
	"testing"
	"time"
)

func TestKeyPool_MarkOutcomeFailover(t *testing.T) {
	p := NewKeyPool([]string{"k1", "k2"})

	// Round-robin hands out k1 then k2.
	if g := p.Get(); g != "k1" {
		t.Fatalf("first Get want k1, got %q", g)
	}

	// k1 dies (auth error) → it must drop out of rotation.
	p.MarkOutcome("k1", 401, 0)
	for i := 0; i < 4; i++ {
		if g := p.Get(); g != "k2" {
			t.Fatalf("after k1 dead, Get want k2, got %q", g)
		}
	}

	// 429 puts a key into cooldown (exhausted), still removed from rotation.
	p.MarkOutcome("k2", 429, 30*time.Second)

	// Success revives a key.
	p.MarkOutcome("k2", 200, 0)
}

func TestKeyPool_MarkOutcomeNilSafe(t *testing.T) {
	var p *KeyPool
	p.MarkOutcome("k", 429, 0) // must not panic on nil receiver
	NewKeyPool([]string{"a"}).MarkOutcome("", 401, 0)
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	if d := ParseRetryAfter(h); d != 0 {
		t.Fatalf("absent header want 0, got %v", d)
	}
	h.Set("Retry-After", "7")
	if d := ParseRetryAfter(h); d != 7*time.Second {
		t.Fatalf("want 7s, got %v", d)
	}
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if d := ParseRetryAfter(h); d != 0 {
		t.Fatalf("HTTP-date form want 0 (fallback), got %v", d)
	}
}
