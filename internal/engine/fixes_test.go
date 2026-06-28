package engine

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
)

func TestClipRunes_NoUTF8Corruption(t *testing.T) {
	s := strings.Repeat("中文", 200) // 400 runes, 1200 bytes
	out := clipRunes(s, 100)
	if !utf8.ValidString(out) {
		t.Fatal("clipRunes produced invalid UTF-8")
	}
	// 100 runes + "..." — must not cut a multi-byte rune.
	if got := utf8.RuneCountInString(strings.TrimSuffix(out, "...")); got != 100 {
		t.Fatalf("want 100 runes kept, got %d", got)
	}
	if s2 := clipRunes("short", 100); s2 != "short" {
		t.Fatalf("short string should be unchanged, got %q", s2)
	}
}

func TestToolTargetPath_Aliases(t *testing.T) {
	cases := []map[string]any{
		{"filePath": "a/b.go"},
		{"file_path": "a/b.go"},
		{"path": "a/b.go"},
		{"file": "a/b.go"},
	}
	for _, in := range cases {
		if got := toolTargetPath(in); got != "a/b.go" && got != "a\\b.go" {
			t.Fatalf("alias %v: want normalized a/b.go, got %q", in, got)
		}
	}
	// Same file via different spellings must normalize equal (the parallel-write
	// race hinges on this).
	if toolTargetPath(map[string]any{"filePath": "./x/../a.go"}) != toolTargetPath(map[string]any{"path": "a.go"}) {
		t.Fatal("normalized paths should compare equal")
	}
	if toolTargetPath(map[string]any{"command": "ls"}) != "" {
		t.Fatal("no path key should yield empty string")
	}
}

func TestMasker_DoesNotReMaskPlaceholder(t *testing.T) {
	m := NewToolOutputMasker()
	m.protectionThreshold = 10  // tiny, so almost nothing is protected
	m.minPrunableThreshold = 10 // tiny, so masking triggers easily
	big := strings.Repeat("x", 8000)
	history := []api.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", Name: "bash", Content: big},
		{Role: "tool", Name: "bash", Content: big},
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "done"},
	}
	res1, masked := m.Mask(history, nil)
	if res1.MaskedCount == 0 {
		t.Fatal("expected first pass to mask something")
	}
	// Second pass over already-masked history must not re-mask the placeholders.
	res2, _ := m.Mask(masked, nil)
	if res2.MaskedCount != 0 {
		t.Fatalf("second pass re-masked %d already-masked messages", res2.MaskedCount)
	}
}
