package command

import "testing"

func TestExpandArguments(t *testing.T) {
	cases := []struct{ name, body, args, want string }{
		// Claude-format command files reference their arguments; they used to
		// get the literal placeholder plus the arguments appended below it.
		{"placeholder", "Fix issue $ARGUMENTS and add a test.", "#42", "Fix issue #42 and add a test."},
		{"every placeholder", "$ARGUMENTS / $ARGUMENTS", "x", "x / x"},
		{"placeholder without args", "Review $ARGUMENTS", "", "Review "},
		// Files without the placeholder keep the old behaviour.
		{"append", "Summarise the diff.", "only docs", "Summarise the diff.\n\nonly docs"},
		{"no args", "Summarise the diff.", "  ", "Summarise the diff."},
	}
	for _, c := range cases {
		if got := ExpandArguments(c.body, c.args); got != c.want {
			t.Errorf("%s: ExpandArguments(%q, %q) = %q, want %q", c.name, c.body, c.args, got, c.want)
		}
	}
}
