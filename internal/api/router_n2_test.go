package api

import (
	"context"
	"strings"
	"testing"
)

// Length is counted in characters: 900 Chinese characters (2700 bytes) used to
// cross the 2000-byte hard ceiling and force the premium model.
func TestRouterCountsLengthInCharacters(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	d := mr.Route(context.Background(), strings.Repeat("改", 900))
	if d.Model != "fast" {
		t.Fatalf("900 characters should not hit the hard ceiling: %+v", d)
	}
}

// 0.35 is reachable by length + file scope without a keyword (0.40 was not).
func TestRouterThreshold035Reachable(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	msg := strings.Repeat("整理", 500) + " main.go handler.go util.go"
	if d := mr.Route(context.Background(), msg); d.Model != "premium" {
		t.Fatalf("length(1.0)+files(3) should route premium at 0.35: %+v", d)
	}
}

func TestRoutedModelLabel(t *testing.T) {
	same := NewModelRouter("m", "m")
	same.Route(context.Background(), "hi")
	if l := same.RoutedModelLabel(); l != "" {
		t.Fatalf("fast==main should give no label, got %q", l)
	}
	none := NewModelRouter("m", "")
	none.Route(context.Background(), "hi")
	if l := none.RoutedModelLabel(); l != "" {
		t.Fatalf("no fast model should give no label, got %q", l)
	}
	mr := NewModelRouter("premium", "fast")
	if l := mr.RoutedModelLabel(); l != "" {
		t.Fatalf("before any route: %q", l)
	}
	mr.Route(context.Background(), "hi")
	if l := mr.RoutedModelLabel(); l != "fast" {
		t.Fatalf("label = %q, want fast", l)
	}
	mr.Route(context.Background(), "please refactor the architecture")
	if l := mr.RoutedModelLabel(); l != "premium" {
		t.Fatalf("label = %q, want premium", l)
	}
}
