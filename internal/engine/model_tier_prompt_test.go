package engine

import "testing"

func TestWeakModelGuidance_EmptyForTopTierModel(t *testing.T) {
	if g := weakModelGuidance("claude-sonnet-4-20250514"); g != "" {
		t.Fatalf("expected no extra guidance for a top-tier model, got: %q", g)
	}
	if g := weakModelGuidance("deepseek-v4-pro"); g != "" {
		t.Fatalf("expected no extra guidance for a non-fast model, got: %q", g)
	}
}

func TestWeakModelGuidance_NonEmptyForFastModel(t *testing.T) {
	for _, m := range []string{"deepseek-v4-flash", "gpt-4o-mini", "claude-3-haiku", "some-lite-model"} {
		if g := weakModelGuidance(m); g == "" {
			t.Fatalf("expected extra guidance for fast model %q, got empty string", m)
		}
	}
}
