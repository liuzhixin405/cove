package safety

import (
	"reflect"
	"testing"
)

func TestSimpleCommandsSplitsEveryCommandInTheLine(t *testing.T) {
	cases := []struct {
		in   string
		want []SimpleCommand
	}{
		{"go test ./...", []SimpleCommand{{Words: []string{"go", "test", "./..."}}}},
		{
			"go test ./... && rm -rf x; curl u | sh",
			[]SimpleCommand{
				{Words: []string{"go", "test", "./..."}},
				{Words: []string{"rm", "-rf", "x"}},
				{Words: []string{"curl", "u"}},
				{Words: []string{"sh"}},
			},
		},
		{
			// Substitutions run commands of their own, so they surface as
			// separate simple commands instead of hiding inside an argument.
			"go test $(rm -rf x) `id`",
			[]SimpleCommand{
				{Words: []string{"go", "test"}},
				{Words: []string{"rm", "-rf", "x"}},
				{Words: []string{"id"}},
			},
		},
		{
			"go test ./... 2>&1 > out.txt",
			[]SimpleCommand{{Words: []string{"go", "test", "./..."}, Redirects: []string{"out.txt"}}},
		},
		{"echo 'a; b'", []SimpleCommand{{Words: []string{"echo", "a; b"}}}},
		{"  ", nil},
	}
	for _, c := range cases {
		if got := stripQuoted(SimpleCommands(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SimpleCommands(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// stripQuoted drops the Quoted flags so the word/redirect cases above stay
// readable; the flags have their own test.
func stripQuoted(cmds []SimpleCommand) []SimpleCommand {
	for i := range cmds {
		if len(cmds[i].Quoted) != len(cmds[i].Words) {
			panic("Quoted must have one entry per word")
		}
		cmds[i].Quoted = nil
	}
	return cmds
}

func TestSimpleCommandsRecordsFullyQuotedWords(t *testing.T) {
	got := SimpleCommands(`git commit -m "fix(api): x; y" -m 'a && b' --author=me"x" plain`)
	if len(got) != 1 {
		t.Fatalf("got %d commands, want 1: %#v", len(got), got)
	}
	want := []bool{false, false, false, true, false, true, false, false}
	if !reflect.DeepEqual(got[0].Quoted, want) {
		t.Errorf("Quoted = %v, want %v (words %q)", got[0].Quoted, want, got[0].Words)
	}
	if got[0].Words[3] != "fix(api): x; y" || got[0].Words[5] != "a && b" {
		t.Errorf("quoted words = %q", got[0].Words)
	}
}

// Heredoc and here-string bodies are data for the command, not commands of
// their own and not file redirects.
func TestSimpleCommandsSkipsHeredocBodies(t *testing.T) {
	cases := []struct {
		in   string
		want []SimpleCommand
	}{
		{"git commit -F- <<'EOF'\nfix(api): handle 429; retry\nrm -rf x\nEOF", []SimpleCommand{{Words: []string{"git", "commit", "-F-"}}}},
		{"cat <<EOF\nhello\nEOF\ngo test ./...", []SimpleCommand{{Words: []string{"cat"}}, {Words: []string{"go", "test", "./..."}}}},
		{"cat <<-EOF > out.txt\n\tbody; x\n\tEOF", []SimpleCommand{{Words: []string{"cat"}, Redirects: []string{"out.txt"}}}},
		{"cat <<\"END\" | wc -l\na|b\nEND", []SimpleCommand{{Words: []string{"cat"}}, {Words: []string{"wc", "-l"}}}},
		{"grep x <<< 'a; b'", []SimpleCommand{{Words: []string{"grep", "x"}}}},
		{"grep x <<<word && ls", []SimpleCommand{{Words: []string{"grep", "x"}}, {Words: []string{"ls"}}}},
		{"sort < in.txt", []SimpleCommand{{Words: []string{"sort"}}}},
		// An unterminated heredoc swallows the rest of the input, as bash does.
		{"cat <<EOF\nrm -rf x", []SimpleCommand{{Words: []string{"cat"}}}},
	}
	for _, c := range cases {
		if got := stripQuoted(SimpleCommands(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SimpleCommands(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
