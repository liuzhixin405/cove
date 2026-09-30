package tool

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// extractSnippetNearLink replaced "<a " with a space before stripping tags,
// which broke the tag it was part of: `<a class="result__snippet" href="…">`
// became ` class="result__snippet" href="…">`, no longer a tag, and the
// snippet carried the raw attribute text.
func TestExtractSnippetNearLinkStripsTags(t *testing.T) {
	link := `<a class="result__a" href="https://example.com/">Example</a>`
	body := `<div class="result">` + link +
		`<a class="result__snippet" href="https://example.com/x">An <b>example</b> snippet &amp; more</a>` +
		`<div class="result__extras"><span class="url">example.com</span></div>`
	got := extractSnippetNearLink(body, link)
	for _, bad := range []string{"class=", "href=", "result__", `">`, "<", ">"} {
		if strings.Contains(got, bad) {
			t.Fatalf("snippet has markup %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "An example snippet & more") {
		t.Fatalf("snippet = %q", got)
	}
}

// The window is cut at a fixed length, so it can end inside a tag; that
// half tag is not text.
func TestExtractSnippetNearLinkDropsTagCutByTheWindow(t *testing.T) {
	link := `<a href="https://example.com/">Example</a>`
	text := strings.Repeat("word ", 70) // 350 bytes
	body := link + text + `<a class="result__snippet" href="https://example.com/a-long-path">next</a>`
	got := extractSnippetNearLink(body, link)
	if strings.Contains(got, "<") || strings.Contains(got, "class") || strings.Contains(got, "href") {
		t.Fatalf("half tag in snippet: %q", got)
	}
	if !strings.HasPrefix(got, "word word") {
		t.Fatalf("snippet = %q", got)
	}
}

// The window was cut at a byte offset, so when it landed inside a multi-byte
// rune the snippet ended in a partial character (a lone "\xe4"): invalid
// UTF-8 that providers' JSON encoders reject.
func TestExtractSnippetNearLinkKeepsRunesWhole(t *testing.T) {
	link := `<a href="https://example.com/">Example</a>`
	body := link + strings.Repeat("中", 200) // 600 bytes; 400 falls inside a rune
	got := extractSnippetNearLink(body, link)
	if !utf8.ValidString(got) {
		t.Fatalf("snippet is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "中中") {
		t.Fatalf("snippet = %q", got)
	}
}
