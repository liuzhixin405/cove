package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// truncate output is fed straight into styles that set a fixed Width, and
// lipgloss wraps anything wider onto a second line. A chrome row that grows
// from one line to two pushes the entire frame down, which is what makes the
// layout appear to fall apart mid-run.
func TestTruncateNeverExceedsMax(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"plain ascii", "hello world this is long", 8},
		{"cjk", "中文测试内容很长很长需要截断", 5},
		{"cjk odd limit", "中文测试内容很长很长需要截断", 7},
		{"exactly max", "exactly8", 8},
		{"max of one", "hello", 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncate(c.in, c.max)
			if w := lipgloss.Width(got); w > c.max {
				t.Fatalf("truncate(%q, %d) = %q, width %d exceeds max", c.in, c.max, got, w)
			}
		})
	}
}

// Styled strings reach truncate already carrying ANSI colour codes. Cutting one
// in half leaves the terminal mid-escape, and it then swallows whatever follows
// — corrupting the rest of the frame, not just this row.
func TestTruncateNeverSplitsAnEscapeSequence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"colour wraps whole string", "\x1b[31mhello world this is long\x1b[0m", 8},
		{"cut lands inside the opening escape", "\x1b[1;32mcove v9.0.5 · flash · auto\x1b[0m", 12},
		{"cut lands just after the escape", "\x1b[38;5;208mhello\x1b[0m world again", 3},
		{"nested reset", "\x1b[1m\x1b[31mbold red text here\x1b[0m", 6},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncate(c.in, c.max)
			if bad := danglingEscape(got); bad != "" {
				t.Fatalf("truncate(%q, %d) = %q leaves a truncated escape sequence %q", c.in, c.max, got, bad)
			}
			if w := lipgloss.Width(got); w > c.max {
				t.Fatalf("truncate(%q, %d) = %q, width %d exceeds max", c.in, c.max, got, w)
			}
		})
	}
}

// danglingEscape returns the trailing fragment of an ANSI escape sequence that
// was cut off before its final byte, or "" when every sequence is complete.
func danglingEscape(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		j := i + 1
		if j >= len(s) || s[j] != '[' {
			return s[i:]
		}
		j++
		for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
			j++
		}
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j >= len(s) || s[j] < 0x40 || s[j] > 0x7e {
			return s[i:]
		}
		i = j
	}
	return ""
}
