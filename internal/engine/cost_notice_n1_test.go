package engine

import (
	"strings"
	"testing"
)

// Reaching 80% of max_budget_usd is said once; changing the budget re-arms it.
func TestCostBudgetNoticeAtEightyPercentOnce(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	var lines []string
	eng.OnEngineOutput = func(s string) { lines = append(lines, s) }
	eng.SetMaxBudget(1)
	eng.costTracker.AddDetailed("deepseek-chat", 0, 0, 0, 0)
	eng.costBudgetNotice()
	if len(lines) != 0 {
		t.Fatalf("notice below 80%%: %q", lines)
	}
	for eng.costTracker.Totals().Cost < 0.8 {
		eng.costTracker.AddDetailed("claude-opus-4", 100000, 100000, 0, 0)
	}
	eng.costBudgetNotice()
	eng.costBudgetNotice()
	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "80%") != 1 {
		t.Fatalf("want one 80%% notice, got %q", lines)
	}
	eng.SetMaxBudget(100)
	eng.costBudgetNotice()
	if strings.Count(strings.Join(lines, "\n"), "80%") != 1 {
		t.Fatalf("notice under a raised budget: %q", lines)
	}
	eng.SetMaxBudget(eng.costTracker.Totals().Cost * 1.1)
	eng.costBudgetNotice()
	if strings.Count(strings.Join(lines, "\n"), "80%") != 2 {
		t.Fatalf("new budget did not re-arm the notice: %q", lines)
	}
}
