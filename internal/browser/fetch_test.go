package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// localBrowser returns a Browser allowed to reach the loopback test server,
// which is the only way to exercise fetch() without touching the network.
func localBrowser(t *testing.T, tune func(*Config)) *Browser {
	t.Helper()
	cfg := DefaultConfig()
	cfg.AllowLocalhost = true
	if tune != nil {
		tune(&cfg)
	}
	return New(cfg)
}

func serveBody(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetch_ConvertsOnlyHTMLContentType is the routing decision fetch() makes:
// the requested format is applied to an HTML response, while a non-HTML body is
// returned byte-for-byte even when it happens to contain angle brackets.
func TestFetch_ConvertsOnlyHTMLContentType(t *testing.T) {
	const page = `<html><body><h1>Title</h1><p>Hello <a href="https://example.com/x">link</a></p></body></html>`
	b := localBrowser(t, nil)

	t.Run("html to text", func(t *testing.T) {
		srv := serveBody(t, "text/html; charset=utf-8", page)
		res, err := b.FetchHeadless(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("FetchHeadless: %v", err)
		}
		if res.Content != "Title\nHello link" {
			t.Errorf("content = %q, want %q", res.Content, "Title\nHello link")
		}
		if res.Format != "text" {
			t.Errorf("format = %q, want text", res.Format)
		}
		if res.StatusCode != 200 || res.URL != srv.URL {
			t.Errorf("status/url = %d/%q, want 200/%q", res.StatusCode, res.URL, srv.URL)
		}
	})

	t.Run("html to markdown", func(t *testing.T) {
		srv := serveBody(t, "application/xhtml+xml", page)
		res, err := b.FetchMarkdown(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("FetchMarkdown: %v", err)
		}
		want := "# Title\n\nHello [link](https://example.com/x)"
		if res.Content != want {
			t.Errorf("content = %q, want %q", res.Content, want)
		}
		if res.Format != "markdown" {
			t.Errorf("format = %q, want markdown", res.Format)
		}
	})

	t.Run("html format keeps markup", func(t *testing.T) {
		srv := serveBody(t, "text/html", page)
		res, err := b.FetchHTML(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("FetchHTML: %v", err)
		}
		if res.Content != page {
			t.Errorf("content = %q, want the raw page", res.Content)
		}
	})

	t.Run("non-html body is not converted", func(t *testing.T) {
		srv := serveBody(t, "text/plain; charset=utf-8", page)
		res, err := b.FetchHeadless(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("FetchHeadless: %v", err)
		}
		if res.Content != page {
			t.Errorf("text/plain body was rewritten: got %q", res.Content)
		}
	})
}

// TestFetch_PropagatesErrorStatus: a 404 must come back as a result the caller
// can inspect, not as a Go error, because the body often explains the failure.
func TestFetch_PropagatesErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such page"))
	}))
	defer srv.Close()

	res, err := localBrowser(t, nil).FetchHeadless(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchHeadless on 404: %v", err)
	}
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	if res.Content != "no such page" {
		t.Errorf("content = %q, want the error body", res.Content)
	}
}

// TestFetch_BlockedBeforeAnyRequest: validation runs before the client is used,
// so a disallowed URL never produces a request at all.
func TestFetch_BlockedBeforeAnyRequest(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	b := New(DefaultConfig()) // AllowLocalhost=false
	if _, err := b.FetchHeadless(context.Background(), srv.URL); err == nil {
		t.Fatal("fetch of a loopback URL succeeded with localhost disallowed")
	}
	if _, err := b.FetchHeadless(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("fetch of a file:// URL succeeded")
	}
	if hits != 0 {
		t.Fatalf("blocked fetch still reached the server %d time(s)", hits)
	}
}

// TestFetch_MaxBodySizeCapsOnRuneBoundary: maxBodySize is a byte budget, and a
// budget that lands inside a multi-byte character must not leave a half rune in
// the result — invalid UTF-8 renders as U+FFFD and can be rejected outright by
// the provider's JSON encoder.
func TestFetch_MaxBodySizeCapsOnRuneBoundary(t *testing.T) {
	// "世" is 3 bytes; a 10-byte cap lands one byte into the 4th character.
	srv := serveBody(t, "text/plain; charset=utf-8", strings.Repeat("世", 5))
	b := localBrowser(t, func(c *Config) { c.MaxBodySize = 10 })

	res, err := b.FetchHeadless(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchHeadless: %v", err)
	}
	if !utf8.ValidString(res.Content) {
		t.Errorf("maxBodySize cut a rune in half: %q is not valid UTF-8", res.Content)
	}
	if res.Content != strings.Repeat("世", 3) {
		t.Errorf("content = %q, want the 3 whole characters that fit in 10 bytes", res.Content)
	}
}

// TestFetch_OutputLimitCapsOnRuneBoundary: the 100000-byte output limit is not a
// multiple of 3, so a CJK page is guaranteed to hit the mid-rune case.
func TestFetch_OutputLimitCapsOnRuneBoundary(t *testing.T) {
	const runes = 40000 // 120000 bytes of "世", well over the 100000-byte limit
	srv := serveBody(t, "text/plain; charset=utf-8", strings.Repeat("世", runes))

	res, err := localBrowser(t, nil).FetchHeadless(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchHeadless: %v", err)
	}
	if !utf8.ValidString(res.Content) {
		t.Fatalf("output limit cut a rune in half: result is not valid UTF-8 (len=%d)", len(res.Content))
	}
	if !strings.Contains(res.Content, "truncated from 120000 bytes") {
		tail := res.Content
		if len(tail) > 80 {
			tail = tail[len(tail)-80:]
		}
		t.Fatalf("missing truncation notice; result ends with %q", tail)
	}
	kept := res.Content[:strings.Index(res.Content, "\n... [truncated")]
	// 100000 bytes holds 33333 whole 3-byte characters (99999 bytes).
	if kept != strings.Repeat("世", 33333) {
		t.Errorf("kept %d bytes / %d runes of text, want 33333 whole 世 runes", len(kept), utf8.RuneCountInString(kept))
	}
}

// TestTrimPartialTrailingRune guards the narrow contract of the body-cap
// repair: strip a half-written character at the very end, and touch nothing
// else — a body that simply is not UTF-8 must be passed through unchanged.
func TestTrimPartialTrailingRune(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"ascii", "hello", "hello"},
		{"complete cjk", "世界", "世界"},
		{"one byte of a 3-byte rune", "世\xe4", "世"},
		{"two bytes of a 3-byte rune", "世\xe4\xb8", "世"},
		{"three bytes of a 4-byte rune", "a\xf0\x9f\x92", "a"},
		{"complete 4-byte rune", "a\U0001F600", "a\U0001F600"},
		{"replacement char is a real rune", "a�", "a�"},
		{"non-utf8 tail byte is left alone", "text\xff", "text\xff"},
		{"invalid bytes mid-string are left alone", "a\xffb", "a\xffb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := trimPartialTrailingRune(tt.in); got != tt.want {
				t.Fatalf("trimPartialTrailingRune(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFetchRendered_ValidatesBeforeRendering: URL validation must happen before
// the headless path is entered, so a bad URL is reported as a bad URL rather
// than as "Chrome unavailable" (or, in a chromedp build, actually navigated to).
func TestFetchRendered_ValidatesBeforeRendering(t *testing.T) {
	b := New(DefaultConfig())

	if _, err := b.FetchRendered(context.Background(), "file:///etc/passwd", "text"); err == nil ||
		!strings.Contains(err.Error(), "unsupported scheme") {
		t.Errorf("FetchRendered(file://) error = %v, want an unsupported-scheme error", err)
	}
	if _, err := b.FetchRendered(context.Background(), "169.254.169.254/latest/meta-data/", "text"); err == nil ||
		!strings.Contains(err.Error(), "blocked") {
		t.Errorf("FetchRendered(metadata host) error = %v, want a blocked-host error", err)
	}
	if _, err := b.Screenshot(context.Background(), "127.0.0.1:9000/x"); err == nil ||
		!strings.Contains(err.Error(), "blocked") {
		t.Errorf("Screenshot(loopback) error = %v, want a blocked-host error", err)
	}

	// A permitted URL gets past validation and then fails on the build tag,
	// which also proves the scheme-less form above was normalized to https
	// rather than rejected outright.
	if chromeAvailable() {
		return // -tags chromedp: a real Chrome would be launched
	}
	_, err := b.FetchRendered(context.Background(), publicIP+"/page", "text")
	if !errors.Is(err, ErrChromeUnavailable) {
		t.Errorf("FetchRendered on a public host = %v, want ErrChromeUnavailable in a non-chromedp build", err)
	}
	if b.ChromeAvailable() {
		t.Error("ChromeAvailable() is true in a build without the chromedp tag")
	}
}
