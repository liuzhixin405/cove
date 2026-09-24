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
		if got := SimpleCommands(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SimpleCommands(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
