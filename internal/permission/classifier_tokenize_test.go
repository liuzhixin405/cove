package permission

import (
	"testing"

	"github.com/liuzhixin405/cove/internal/safety"
)

// countTokenize swaps the classifier's tokenizer for a counting wrapper.
func countTokenize(tb testing.TB) *int {
	tb.Helper()
	n := new(int)
	orig := simpleCommands
	simpleCommands = func(command string) []safety.SimpleCommand {
		*n++
		return orig(command)
	}
	tb.Cleanup(func() { simpleCommands = orig })
	return n
}

// IsReadOnlyLineFor classifies the line and then checks for curl/wget on the
// same simple commands: the line is split once, not twice.
func TestIsReadOnlyLineTokenizesOnce(t *testing.T) {
	n := countTokenize(t)
	c := NewClassifier()
	if !c.IsReadOnlyLineFor("git status && git diff | grep x", ShellPOSIX) {
		t.Fatal("line should be read-only")
	}
	if *n != 1 {
		t.Fatalf("IsReadOnlyLineFor split the line %d times, want 1", *n)
	}
}

// BenchmarkIsReadOnlyLineFor reports the tokenizer calls per check
// (tokenize/op), which should stay at 1.
func BenchmarkIsReadOnlyLineFor(b *testing.B) {
	n := countTokenize(b)
	c := NewClassifier()
	line := "git status && git log --oneline -5 | head -3; ls -la"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.IsReadOnlyLineFor(line, ShellPOSIX)
	}
	b.ReportMetric(float64(*n)/float64(b.N), "tokenize/op")
}
