package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/session"
)

func userMsg(content string) api.Message {
	return api.Message{Role: "user", Content: content}
}

func syntheticMsg(content string) api.Message {
	return api.Message{Role: "user", Content: content, Synthetic: true}
}

// ---------- compactRunes ----------

func TestCompactRunesClipsOnRuneBoundary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"chinese over limit", strings.Repeat("配", 100), 10},
		{"mixed over limit", "abc" + strings.Repeat("配置", 50), 7},
		{"emoji over limit", strings.Repeat("🙂", 40), 5},
		{"exactly at limit", strings.Repeat("配", 10), 10},
		{"under limit", "短", 10},
		{"zero max", "配置文件", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compactRunes(tc.in, tc.max)
			if !utf8.ValidString(got) {
				t.Fatalf("compactRunes(%q, %d) = %q, not valid UTF-8", tc.in, tc.max, got)
			}
			body := strings.TrimSuffix(got, "...")
			if n := utf8.RuneCountInString(body); n > tc.max {
				t.Fatalf("body has %d runes, want at most %d (got %q)", n, tc.max, got)
			}
		})
	}
}

func TestCompactRunesTrimsAndAppendsEllipsis(t *testing.T) {
	if got := compactRunes("  hello  ", 20); got != "hello" {
		t.Fatalf("compactRunes = %q, want %q", got, "hello")
	}
	got := compactRunes("abcdefghij", 5)
	if got != "abcde..." {
		t.Fatalf("compactRunes = %q, want %q", got, "abcde...")
	}
}

// ---------- sessionPreview ----------

// TestSessionPreviewKeepsValidUTF8 is the regression test for the preview being
// byte-sliced at 50: the string is the session label in the Ctrl+S history
// overlay, so a Chinese title rendered as mojibake in the picker.
func TestSessionPreviewKeepsValidUTF8(t *testing.T) {
	r := session.Record{
		Messages: []api.Message{userMsg(strings.Repeat("重构配置加载逻辑", 20))},
	}
	got := sessionPreview(r)
	if got == "" {
		t.Fatal("sessionPreview returned empty for a real user message")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("sessionPreview = %q, not valid UTF-8", got)
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "...")); n > 50 {
		t.Fatalf("preview has %d runes, want at most 50", n)
	}
}

func TestSessionPreviewPrefersStoredPreview(t *testing.T) {
	r := session.Record{
		Preview:  "stored preview",
		Messages: []api.Message{userMsg("first user message")},
	}
	if got := sessionPreview(r); got != "stored preview" {
		t.Fatalf("sessionPreview = %q, want the stored preview", got)
	}
}

func TestSessionPreviewIgnoresSyntheticAndEmpty(t *testing.T) {
	// A stored preview that is itself synthetic must be rejected in favour of a
	// real user message.
	r := session.Record{
		Preview: "[system: loop detected]",
		Messages: []api.Message{
			syntheticMsg("[用户指引] do better"),
			{Role: "assistant", Content: "sure"},
			userMsg("the real request"),
		},
	}
	if got := sessionPreview(r); got != "the real request" {
		t.Fatalf("sessionPreview = %q, want %q", got, "the real request")
	}

	// Nothing usable at all.
	empty := session.Record{Messages: []api.Message{
		syntheticMsg("[system: x]"),
		{Role: "assistant", Content: "hello"},
	}}
	if got := sessionPreview(empty); got != "" {
		t.Fatalf("sessionPreview = %q, want empty when there is no real user text", got)
	}
}

// ---------- deriveCleanTitle ----------

func TestDeriveCleanTitle(t *testing.T) {
	t.Run("first substantive user message wins", func(t *testing.T) {
		msgs := []api.Message{
			syntheticMsg("[system: nudge]"),
			userMsg("继续"), // trivial, must be skipped
			userMsg("给配置加载加上 profile 支持"),
			userMsg("another later message"),
		}
		if got := deriveCleanTitle(msgs); got != "给配置加载加上 profile 支持" {
			t.Fatalf("deriveCleanTitle = %q", got)
		}
	})

	t.Run("newlines collapsed", func(t *testing.T) {
		got := deriveCleanTitle([]api.Message{userMsg("line one\nline two")})
		if strings.Contains(got, "\n") {
			t.Fatalf("deriveCleanTitle = %q, want newlines collapsed", got)
		}
	})

	t.Run("falls back to the longest low-signal message", func(t *testing.T) {
		// Every message here is trivial/low-signal (each is in the trivial set
		// or prefixed "继续"), so the first pass finds nothing and the fallback
		// picks the longest candidate by rune count.
		msgs := []api.Message{userMsg("hi"), userMsg("继续继续继续"), userMsg("好的")}
		got := deriveCleanTitle(msgs)
		if got == "" {
			t.Fatal("deriveCleanTitle returned empty, want the longest fallback")
		}
		if got != "继续继续继续" {
			t.Fatalf("deriveCleanTitle = %q, want the longest candidate", got)
		}
	})

	t.Run("a short but non-trivial message is accepted as-is", func(t *testing.T) {
		// Pins current behaviour: only words in the trivial set are skipped, so
		// a two-letter word like "ok" is taken as a real title. This is the
		// first-pass path, not the fallback.
		if got := deriveCleanTitle([]api.Message{userMsg("hi"), userMsg("ok")}); got != "ok" {
			t.Fatalf("deriveCleanTitle = %q, want %q", got, "ok")
		}
	})

	t.Run("no usable message", func(t *testing.T) {
		msgs := []api.Message{
			syntheticMsg("[system: x]"),
			{Role: "assistant", Content: "reply"},
			{Role: "tool", Content: "output"},
		}
		if got := deriveCleanTitle(msgs); got != "" {
			t.Fatalf("deriveCleanTitle = %q, want empty", got)
		}
	})

	t.Run("result is rune-clipped and valid UTF-8", func(t *testing.T) {
		got := deriveCleanTitle([]api.Message{userMsg(strings.Repeat("重构", 100))})
		if !utf8.ValidString(got) {
			t.Fatalf("deriveCleanTitle = %q, not valid UTF-8", got)
		}
		if n := utf8.RuneCountInString(strings.TrimSuffix(got, "...")); n > 60 {
			t.Fatalf("title has %d runes, want at most 60", n)
		}
	})
}

// ---------- countUserTurns ----------

func TestCountUserTurnsExcludesEngineInjected(t *testing.T) {
	msgs := []api.Message{
		userMsg("real one"),
		{Role: "assistant", Content: "reply"},
		{Role: "tool", Content: "tool output"},
		syntheticMsg("injected via flag"),
		userMsg("[system: 检测到重复循环]"), // synthetic by content, not flagged
		userMsg("[Conversation Summary] ..."),
		userMsg("real two"),
	}
	if got := countUserTurns(msgs); got != 2 {
		t.Fatalf("countUserTurns = %d, want 2", got)
	}
	if got := countUserTurns(nil); got != 0 {
		t.Fatalf("countUserTurns(nil) = %d, want 0", got)
	}
}

// ---------- looksSyntheticHistoryText ----------

func TestLooksSyntheticHistoryText(t *testing.T) {
	synthetic := []string{
		"", "   ",
		"[system: something]",
		"[Conversation Summary] recap",
		"[系统检测到重复操作循环]\n...",
		"[Context truncated at 100 messages]",
		"[用户指引] please",
		"[Continue the task from here]",
		"[会话摘要] ...",
		"run slow tool", "do something", "slow response",
	}
	for _, s := range synthetic {
		if !looksSyntheticHistoryText(s) {
			t.Errorf("looksSyntheticHistoryText(%q) = false, want true", s)
		}
	}

	real := []string{
		"给配置加载加上 profile 支持",
		"fix the parser",
		"systematic review", // must not match the "[system:" prefix
		"continue the refactor of the loader",
	}
	for _, s := range real {
		if looksSyntheticHistoryText(s) {
			t.Errorf("looksSyntheticHistoryText(%q) = true, want false", s)
		}
	}
}

// ---------- isTrivialResumePrompt / isLowSignalResumeInput ----------

func TestIsTrivialResumePrompt(t *testing.T) {
	trivial := []string{
		"", "  ", "继续", "继续上次的", "Continue", "CONTINUE",
		"hi", "Hello", "你好", "嗯", "好的", "1", "?",
		"/history", "/history 3", "/resume", "/resume abc",
	}
	for _, s := range trivial {
		if !isTrivialResumePrompt(s) {
			t.Errorf("isTrivialResumePrompt(%q) = false, want true", s)
		}
	}

	substantive := []string{
		"给配置加载加上 profile 支持",
		"continue is a word inside a longer sentence",
		"fix the glob matcher",
	}
	for _, s := range substantive {
		if isTrivialResumePrompt(s) {
			t.Errorf("isTrivialResumePrompt(%q) = true, want false", s)
		}
	}
}

func TestIsLowSignalResumeInput(t *testing.T) {
	// A single rune is low signal even when it is not in the trivial list.
	for _, s := range []string{"", " ", "x", "配"} {
		if !isLowSignalResumeInput(s) {
			t.Errorf("isLowSignalResumeInput(%q) = false, want true", s)
		}
	}
	if isLowSignalResumeInput("重构配置加载") {
		t.Error("a substantive request was classified as low signal")
	}
}

// ---------- scoreSessionForResume ----------

func TestScoreSessionForResumeRanking(t *testing.T) {
	empty := session.Record{}
	if got := scoreSessionForResume(empty); got != -100 {
		t.Fatalf("an empty session scored %d, want -100", got)
	}

	// A substantive session with tool use and several assistant turns must
	// outrank a trivial "继续" session — that ordering is the whole point of
	// the function (it picks which session "继续" resumes).
	substantive := session.Record{Messages: []api.Message{
		userMsg("重构配置加载，支持 profile 覆盖，并补测试"),
		{Role: "assistant", Content: "ok"},
		{Role: "tool", Content: "read config.go"},
		{Role: "assistant", Content: "done"},
		{Role: "tool", Content: "test output"},
		{Role: "assistant", Content: "all green"},
	}}
	trivial := session.Record{Messages: []api.Message{
		userMsg("继续"),
		{Role: "assistant", Content: "ok"},
	}}

	sSub, sTriv := scoreSessionForResume(substantive), scoreSessionForResume(trivial)
	if sSub <= sTriv {
		t.Fatalf("substantive scored %d, trivial scored %d — substantive must rank higher", sSub, sTriv)
	}

	// A URL in the request is treated as extra signal.
	withURL := session.Record{Messages: []api.Message{
		userMsg("分析 https://example.com/spec 并实现对应接口"),
		{Role: "assistant", Content: "ok"},
	}}
	withoutURL := session.Record{Messages: []api.Message{
		userMsg("分析这个规格文档并实现对应接口啊啊"),
		{Role: "assistant", Content: "ok"},
	}}
	if scoreSessionForResume(withURL) <= scoreSessionForResume(withoutURL) {
		t.Errorf("a request containing a URL did not score higher (%d vs %d)",
			scoreSessionForResume(withURL), scoreSessionForResume(withoutURL))
	}
}

// ---------- writeFileAtomic ----------

func TestWriteFileAtomicReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "data.json")

	if err := writeFileAtomic(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != `{"v":1}` {
		t.Fatalf("content = %q", got)
	}

	// Overwrite must replace, not append.
	if err := writeFileAtomic(path, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("writeFileAtomic (overwrite): %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != `{"v":2}` {
		t.Fatalf("content after overwrite = %q", got)
	}

	// No temp file may survive — a leftover would be picked up by directory
	// scans elsewhere.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file %q left behind", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries in the directory, want exactly 1", len(entries))
	}
}

// ---------- interrupted draft round-trip ----------

func TestInterruptedDraftRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Nothing saved yet.
	if d, err := loadInterruptedDraft(); err == nil && d != nil {
		t.Fatalf("loadInterruptedDraft returned %+v before anything was saved", d)
	}

	msg := userMsg("重构配置加载，支持 profile")
	if err := saveInterruptedDraft(msg, os.ErrDeadlineExceeded); err != nil {
		t.Fatalf("saveInterruptedDraft: %v", err)
	}

	d, err := loadInterruptedDraft()
	if err != nil {
		t.Fatalf("loadInterruptedDraft: %v", err)
	}
	if d == nil {
		t.Fatal("loadInterruptedDraft returned nil after a save")
	}
	if d.UserContent != msg.Content {
		t.Fatalf("UserContent = %q, want %q", d.UserContent, msg.Content)
	}
	if d.Error == "" {
		t.Error("the request error was not recorded")
	}
	if d.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not set")
	}

	// The persisted file must be valid JSON with intact UTF-8.
	p, _ := interruptedDraftPath()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read draft file: %v", err)
	}
	if !utf8.Valid(raw) {
		t.Fatal("draft file is not valid UTF-8")
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("draft file is not valid JSON: %v", err)
	}

	if err := clearInterruptedDraft(); err != nil {
		t.Fatalf("clearInterruptedDraft: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("draft file still present after clear (err=%v)", err)
	}
	// Clearing twice must be harmless.
	if err := clearInterruptedDraft(); err != nil {
		t.Fatalf("second clearInterruptedDraft: %v", err)
	}
}

// ---------- effectiveHistoryTitle ----------

func TestEffectiveHistoryTitleFallsBackFromPlaceholders(t *testing.T) {
	for _, title := range []string{"", "New session", "[system: injected]"} {
		r := session.Record{
			Title:    title,
			Messages: []api.Message{userMsg("给 glob 加上 ** 支持")},
		}
		got := effectiveHistoryTitle(r)
		if got == title && title != "" {
			t.Errorf("effectiveHistoryTitle kept the placeholder %q", title)
		}
		if got == "" {
			t.Errorf("effectiveHistoryTitle returned empty for placeholder %q", title)
		}
	}

	// A real title is preserved.
	r := session.Record{Title: "重构配置加载", Messages: []api.Message{userMsg("x")}}
	if got := effectiveHistoryTitle(r); got != "重构配置加载" {
		t.Fatalf("effectiveHistoryTitle = %q, want the real title preserved", got)
	}
}
