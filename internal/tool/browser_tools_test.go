package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/liuzhixin405/cove/internal/browser"
)

// TestBrowserCaptureAliasAsksLikeScreenshot: Call accepts "capture" as an
// alias of "screenshot" and writes a PNG either way, but CheckPermissions only
// recognised the literal "screenshot" — so action=capture wrote a file with no
// confirmation at all.
func TestBrowserCaptureAliasAsksLikeScreenshot(t *testing.T) {
	bt := NewBrowserTool()
	tctx := Context{Cwd: t.TempDir()}
	for _, action := range []string{"screenshot", "capture", " Capture "} {
		d := bt.CheckPermissions(Input{"action": action, "url": "https://8.8.8.8/"}, tctx)
		if d.Decision != Ask {
			t.Errorf("action %q: decision = %v (%s), want Ask", action, d.Decision, d.Reason)
		}
	}
}

// TestBrowserScreenshotOutputMustStayInWorkspace: the tool is marked
// read-only, so a read-only sub-agent runs it without any permission gate, and
// "output" is chosen by the model. An absolute path or ../ therefore let a
// "read-only" call overwrite any file the user can write, e.g. a shell rc file.
func TestBrowserScreenshotOutputMustStayInWorkspace(t *testing.T) {
	cwd := t.TempDir()
	bt := NewBrowserTool()
	tctx := Context{Cwd: cwd}

	outside := filepath.Join(filepath.Dir(cwd), "evil.png")
	for _, out := range []string{outside, "../evil.png", "main.go", "shot.png.bat"} {
		in := Input{"action": "screenshot", "url": "https://8.8.8.8/", "output": out}
		if d := bt.CheckPermissions(in, tctx); d.Decision != Deny {
			t.Errorf("output %q: decision = %v (%s), want Deny", out, d.Decision, d.Reason)
		}
		res, err := bt.Call(context.Background(), in, tctx)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Data, "output") {
			t.Errorf("output %q: Call = %q, want an output-path error", out, res.Data)
		}
	}

	for _, out := range []string{"", "shot.png", "shots/Page.PNG", filepath.Join(cwd, "abs.png")} {
		in := Input{"action": "screenshot", "url": "https://8.8.8.8/", "output": out}
		if d := bt.CheckPermissions(in, tctx); d.Decision != Ask {
			t.Errorf("output %q: decision = %v (%s), want Ask", out, d.Decision, d.Reason)
		}
	}
}

// TestBrowserHTTPFallbackFetchesOnce: without headless Chrome, navigate with
// format text or html first fetched the page as markdown and then fetched it
// again in the requested format — two requests (twice the latency, and twice
// any side effect of a GET) for one tool call.
func TestBrowserHTTPFallbackFetchesOnce(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Title</h1><p>hello</p></body></html>"))
	}))
	defer srv.Close()

	bt := &BrowserTool{br: browser.New(browser.Config{AllowLocalhost: true})}
	if bt.br.ChromeAvailable() {
		t.Skip("built with -tags chromedp: navigate renders with Chrome instead of HTTP")
	}
	for _, format := range []string{"text", "markdown", "html"} {
		atomic.StoreInt32(&hits, 0)
		res, err := bt.Call(context.Background(), Input{"action": "navigate", "url": srv.URL, "format": format}, Context{})
		if err != nil || res.IsError {
			t.Fatalf("format %s: %v %s", format, err, res.Data)
		}
		if !strings.Contains(res.Data, "Format: "+format) {
			t.Errorf("format %s: result reports %q", format, res.Data)
		}
		if n := atomic.LoadInt32(&hits); n != 1 {
			t.Errorf("format %s: page fetched %d times, want 1", format, n)
		}
	}
}
