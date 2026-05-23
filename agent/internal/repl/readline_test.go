package repl

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadInputRuneDecodesUTF8(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("你好\r"))

	first, err := readInputRune(r)
	if err != nil {
		t.Fatalf("read first rune: %v", err)
	}
	if first != '你' {
		t.Fatalf("first rune = %q, want %q", first, '你')
	}

	second, err := readInputRune(r)
	if err != nil {
		t.Fatalf("read second rune: %v", err)
	}
	if second != '好' {
		t.Fatalf("second rune = %q, want %q", second, '好')
	}

	third, err := readInputRune(r)
	if err != nil {
		t.Fatalf("read third rune: %v", err)
	}
	if third != '\r' {
		t.Fatalf("third rune = %q, want carriage return", third)
	}
}
