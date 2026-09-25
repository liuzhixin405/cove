package safety

import "testing"

// Skipping a here-document body is only safe when bash really starts one:
// otherwise the "body" lines are commands bash runs. Each case below hides
// "rm -rf x" from a naive tokenizer; it must surface as a command.
func TestHeredocSkippingNeverHidesCommandsBashRuns(t *testing.T) {
	for _, in := range []string{
		"cat \\<<EOF\nrm -rf x\nEOF",              // \< is a literal <, not a heredoc
		"ls # <<EOF\nrm -rf x\nEOF",               // a comment, not a heredoc
		"(( ls<<2 ))\nrm -rf x\n2",                // arithmetic shift
		"(cat <<EOF\nrm -rf x\nEOF\n)",            // inside a subshell: not trusted
		"cat <<E\\OF\nbody\nEOF\nrm -rf x\nE\\OF", // bash's delimiter is EOF
	} {
		found := false
		for _, c := range SimpleCommands(in) {
			if len(c.Words) >= 2 && c.Words[0] == "rm" && c.Words[1] == "-rf" {
				found = true
			}
		}
		if !found {
			t.Errorf("SimpleCommands(%q) hid the rm command: %#v", in, SimpleCommands(in))
		}
	}
}
