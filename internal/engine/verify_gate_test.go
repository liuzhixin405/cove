package engine

import (
	"context"
	"strings"
	"testing"
)

func TestVerifyGate_DisabledWhenNoCommands(t *testing.T) {
	g := NewVerifyGate(nil, "")
	if g.Enabled() {
		t.Fatalf("expected gate with no commands to be disabled")
	}
	results, passed := g.Run(context.Background())
	if !passed || results != nil {
		t.Fatalf("expected no-op Run() to pass trivially, got passed=%v results=%v", passed, results)
	}
}

func TestVerifyGate_AllPass(t *testing.T) {
	g := NewVerifyGate([]string{"exit 0", "exit 0"}, "")
	if !g.Enabled() {
		t.Fatalf("expected gate with commands to be enabled")
	}
	results, passed := g.Run(context.Background())
	if !passed {
		t.Fatalf("expected all-passing commands to pass, results=%v", results)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestVerifyGate_FailFastStopsAtFirstFailure(t *testing.T) {
	g := NewVerifyGate([]string{"exit 1", "exit 0"}, "")
	results, passed := g.Run(context.Background())
	if passed {
		t.Fatalf("expected failure to be reported")
	}
	if len(results) != 1 {
		t.Fatalf("expected fail-fast to stop after the first command, got %d results", len(results))
	}
	if results[0].Passed {
		t.Fatalf("expected first result to be marked failed")
	}
	if results[0].ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", results[0].ExitCode)
	}
}

func TestVerifyGate_MaxRetriesDefault(t *testing.T) {
	g := NewVerifyGate([]string{"exit 1"}, "")
	if g.MaxRetries() <= 0 {
		t.Fatalf("expected a positive default retry budget")
	}
}

func TestSummary_MentionsFailedCommand(t *testing.T) {
	results := []VerifyResult{
		{Command: "go build ./...", Passed: false, ExitCode: 2, Output: "undefined: foo"},
	}
	s := Summary(results)
	if !strings.Contains(s, "go build ./...") || !strings.Contains(s, "undefined: foo") {
		t.Fatalf("expected summary to mention command and output, got: %s", s)
	}
}
