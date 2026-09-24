package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClipRunesKeepsValidUTF8(t *testing.T) {
	// The exact failure mode being guarded: s[:n] on Chinese text produces
	// invalid UTF-8 (a 3-byte rune cut after 1 or 2 bytes).
	s := "更新配置文件并运行测试以确认修复生效"

	for n := 0; n <= utf8.RuneCountInString(s)+3; n++ {
		got := ClipRunes(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("ClipRunes(%q, %d) = %q, which is not valid UTF-8", s, n, got)
		}
		if c := utf8.RuneCountInString(got); c > n {
			t.Fatalf("ClipRunes(_, %d) returned %d runes, want at most %d", n, c, n)
		}
	}
}

func TestClipRunesNoOpWhenShort(t *testing.T) {
	if got := ClipRunes("短", 10); got != "短" {
		t.Fatalf("ClipRunes = %q, want unchanged", got)
	}
	if got := ClipRunes("", 5); got != "" {
		t.Fatalf("ClipRunes(\"\") = %q, want empty", got)
	}
}

func TestClipRunesAppendsEllipsis(t *testing.T) {
	got := ClipRunes("abcdefghij", 8)
	if got != "abcde..." {
		t.Fatalf("ClipRunes = %q, want %q", got, "abcde...")
	}
}

// TestClipRunesSmallN covers the case that used to panic outright: the old
// implementation computed s[:n-3], which is a negative index for n < 3.
func TestClipRunesSmallN(t *testing.T) {
	for n := -1; n <= 3; n++ {
		got := ClipRunes("abcdef", n)
		if !utf8.ValidString(got) {
			t.Fatalf("ClipRunes(_, %d) = %q, invalid UTF-8", n, got)
		}
		if n > 0 && utf8.RuneCountInString(got) > n {
			t.Fatalf("ClipRunes(_, %d) = %q, too long", n, got)
		}
	}
}

func TestClipBytesRespectsBudgetAndBoundary(t *testing.T) {
	s := "配置文件" // 4 runes, 12 bytes

	for n := 0; n <= 14; n++ {
		got := ClipBytes(s, n, "|cut")
		// The suffix is not counted against the budget, so strip it to check.
		body := got
		if len(got) >= 4 && got[len(got)-4:] == "|cut" {
			body = got[:len(got)-4]
		}
		if !utf8.ValidString(body) {
			t.Fatalf("ClipBytes(_, %d) body = %q, invalid UTF-8", n, body)
		}
		if len(body) > n {
			t.Fatalf("ClipBytes(_, %d) body is %d bytes, over budget", n, len(body))
		}
	}

	if got := ClipBytes(s, 100, "|cut"); got != s {
		t.Fatalf("ClipBytes with ample budget = %q, want unchanged", got)
	}
}

func TestHeadRunes(t *testing.T) {
	s := "abc汉字def"
	if got := HeadRunes(s, 4); got != "abc汉" {
		t.Fatalf("HeadRunes = %q, want %q", got, "abc汉")
	}
	if got := HeadRunes(s, 0); got != "" {
		t.Fatalf("HeadRunes(_, 0) = %q, want empty", got)
	}
	if got := HeadRunes(s, 100); got != s {
		t.Fatalf("HeadRunes past end = %q, want whole string", got)
	}
}

func TestClipMiddleBytesKeepsBothEnds(t *testing.T) {
	s := "HEAD\n" + strings.Repeat("中间内容\n", 5000) + "TAIL exit code: 1"
	got := ClipMiddleBytes(s, 1000)
	if !strings.HasPrefix(got, "HEAD\n") || !strings.HasSuffix(got, "TAIL exit code: 1") {
		t.Fatalf("ClipMiddleBytes lost an end: %q ... %q", got[:20], got[len(got)-20:])
	}
	if !strings.Contains(got, "bytes omitted") {
		t.Fatal("no omission marker")
	}
	if len(got) > 1100 {
		t.Fatalf("len = %d, want about 1000", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("split a rune")
	}
	if ClipMiddleBytes("short", 1000) != "short" {
		t.Fatal("clipped a short string")
	}
}
