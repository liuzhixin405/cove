package safety

import (
	"reflect"
	"testing"
)

// Under bash a backslash-newline joins two lines and a backslash inside an
// unquoted word only quotes the next character. The tokenizer kept both
// literally, so the hard block (the only guard in bypass mode) read these as
// harmless: "rm \<NL>-rf ~" as "rm \" plus a command "-rf ~", "r\m" as a
// program "m" (ProgramName took the backslash for a path separator) and
// "/\u\s\r" as a path that is not /usr.
func TestCatastrophicCommandSeesBashBackslashes(t *testing.T) {
	for _, cmd := range []string{
		"rm \\\n-rf ~",
		"rm -rf \\\n/",
		`r\m -rf /`,
		`rm -rf /\u\s\r`,
		`\rm -rf ~`,
		`rm -r\f /`,
		"bash -c 'r\\m -rf /'",
	} {
		if _, ok := CatastrophicCommand(cmd); !ok {
			t.Errorf("CatastrophicCommand(%q) = false, want true", cmd)
		}
	}
	// PowerShell and cmd paths keep their backslashes and stay allowed.
	for _, cmd := range []string{
		`Remove-Item -Recurse -Force C:\proj\build`,
		`rm -rf .\build\`,
		`del /s /q build\obj`,
	} {
		if why, ok := CatastrophicCommand(cmd); ok {
			t.Errorf("CatastrophicCommand(%q) = true (%s), want false", cmd, why)
		}
	}
}

// SimpleCommandsPOSIX applies bash's line continuation and quote removal.
func TestSimpleCommandsPOSIXBackslashes(t *testing.T) {
	cases := []struct {
		in   string
		want []SimpleCommand
	}{
		{"find . -f\\\nls out.txt", []SimpleCommand{{Words: []string{"find", ".", "-fls", "out.txt"}}}},
		{"git log --output\\\n echo", []SimpleCommand{{Words: []string{"git", "log", "--output", "echo"}}}},
		{`r\m -rf x`, []SimpleCommand{{Words: []string{"rm", "-rf", "x"}}}},
		{`echo a\;b`, []SimpleCommand{{Words: []string{"echo", "a;b"}}}},
		{`echo a\ b`, []SimpleCommand{{Words: []string{"echo", "a b"}}}},
		{`echo "a\"b" 'c\d'`, []SimpleCommand{{Words: []string{"echo", `a"b`, `c\d`}}}},
		{`echo "a\qb\\c\$d"`, []SimpleCommand{{Words: []string{"echo", `a\qb\c$d`}}}},
		{`echo \'x`, []SimpleCommand{{Words: []string{"echo", "'x"}}}},
		{`ls \\`, []SimpleCommand{{Words: []string{"ls", `\`}}}},
		{"echo \"a\\\nb\"", []SimpleCommand{{Words: []string{"echo", "ab"}}}},
		// \\<<EOF is a literal backslash followed by a real heredoc.
		{"cat \\\\<<EOF\nrm -rf x\nEOF", []SimpleCommand{{Words: []string{"cat", `\`}}}},
		// \<<EOF is no heredoc: the next line is a command.
		{"cat \\<<EOF\nrm -rf x", []SimpleCommand{{Words: []string{"cat", "<"}}, {Words: []string{"rm", "-rf", "x"}}}},
	}
	for _, c := range cases {
		if got := stripQuoted(SimpleCommandsPOSIX(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SimpleCommandsPOSIX(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
	// An escaped character is not reported as quoted: a caller that trusts
	// quoted words must still refuse an operator character it cannot see.
	got := SimpleCommandsPOSIX(`echo a\;b`)
	if len(got) != 1 || got[0].Quoted[1] {
		t.Errorf("escaped word reported as fully quoted: %#v", got)
	}
	// The default tokenizer is unchanged: backslashes stay literal.
	if got := stripQuoted(SimpleCommands(`type C:\x\y`)); !reflect.DeepEqual(got, []SimpleCommand{{Words: []string{"type", `C:\x\y`}}}) {
		t.Errorf("SimpleCommands changed backslash handling: %#v", got)
	}
}
