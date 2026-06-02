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

func TestCompletionCycleAdvancesCandidates(t *testing.T) {
	list := []string{"/api-key", "/attach", "/base-url"}

	next, idx, ok := completionCycleNext("/", "/", list, -1)
	if !ok {
		t.Fatal("first cycle did not advance")
	}
	if next != "/api-key" || idx != 0 {
		t.Fatalf("first candidate = (%q, %d), want (/api-key, 0)", next, idx)
	}

	next, idx, ok = completionCycleNext("/api-key", "/", list, idx)
	if !ok {
		t.Fatal("second cycle did not advance")
	}
	if next != "/attach" || idx != 1 {
		t.Fatalf("second candidate = (%q, %d), want (/attach, 1)", next, idx)
	}

	if _, _, ok = completionCycleNext("/custom", "/", list, idx); ok {
		t.Fatal("cycle advanced after user-edited input")
	}
}
