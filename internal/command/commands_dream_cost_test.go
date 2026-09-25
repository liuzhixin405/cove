package command

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/dream"
)

func TestFormatDreamCost(t *testing.T) {
	if s := FormatDreamCost(DreamUsage{}); s != "" {
		t.Fatalf("no usage should print nothing, got %q", s)
	}
	s := FormatDreamCost(DreamUsage{InputTokens: 12345, OutputTokens: 678, CostUSD: 0.0213})
	for _, want := range []string{"上次整理用量", "12,345", "678", "$0.0213"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
	if s := FormatDreamCost(DreamUsage{InputTokens: 10}); strings.Contains(s, "$") {
		t.Errorf("unknown cost should not print a price: %q", s)
	}
}

func TestFormatDreamStatusShowsLastRunCost(t *testing.T) {
	s := FormatDreamStatus(dream.Status{LastRunInputTokens: 2000, LastRunOutputTokens: 300, LastRunCostUSD: 0.01})
	if !strings.Contains(s, "上次整理用量") || !strings.Contains(s, "2,000") || !strings.Contains(s, "$0.0100") {
		t.Fatalf("/dream lacks the cost line:\n%s", s)
	}
}
