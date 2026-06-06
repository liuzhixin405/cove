package token

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEstimateCountsNonASCIIConservatively(t *testing.T) {
	if got := Estimate("hello world"); got != 4 {
		t.Fatalf("Estimate(ascii) = %d, want 4", got)
	}
	if got := Estimate("你好世界"); got != 4 {
		t.Fatalf("Estimate(chinese) = %d, want 4", got)
	}
	if got := Estimate("hello 世界"); got != 4 {
		t.Fatalf("Estimate(mixed) = %d, want 4", got)
	}
}

func TestTruncateToTokensPreservesUTF8(t *testing.T) {
	got := TruncateToTokens("你好世界 hello", 1)
	if !utf8.ValidString(got) {
		t.Fatalf("TruncateToTokens returned invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
}
