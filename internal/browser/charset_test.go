package browser

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func gbk(t *testing.T, s string) string {
	t.Helper()
	b, err := simplifiedchinese.GBK.NewEncoder().String(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFetch_DecodesDeclaredCharset: the body was used as UTF-8 whatever the
// server declared, so a GBK page (still common on Chinese sites) came back as
// mojibake.
func TestFetch_DecodesDeclaredCharset(t *testing.T) {
	b := localBrowser(t, nil)

	t.Run("content-type header", func(t *testing.T) {
		srv := serveBody(t, "text/html; charset=GBK", gbk(t, "<html><body><p>中文页面</p></body></html>"))
		res, err := b.FetchHeadless(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if res.Content != "中文页面" {
			t.Fatalf("content = %q, want 中文页面", res.Content)
		}
	})

	t.Run("meta charset", func(t *testing.T) {
		page := `<html><head><meta http-equiv="Content-Type" content="text/html; charset=gb2312"></head><body><p>政府公告</p></body></html>`
		srv := serveBody(t, "text/html", gbk(t, page))
		res, err := b.FetchHeadless(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if res.Content != "政府公告" {
			t.Fatalf("content = %q, want 政府公告", res.Content)
		}
	})

	t.Run("utf-8 stays as is", func(t *testing.T) {
		srv := serveBody(t, "text/html; charset=utf-8", "<p>你好</p>")
		res, err := b.FetchHeadless(context.Background(), srv.URL)
		if err != nil || res.Content != "你好" {
			t.Fatalf("content = %q, err = %v", res.Content, err)
		}
	})
}

// TestFetch_RefusesBinaryBodies: a PDF or image link was returned as up to
// 100KB of raw bytes — tens of thousands of tokens of noise in the context.
func TestFetch_RefusesBinaryBodies(t *testing.T) {
	b := localBrowser(t, nil)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" + strings.Repeat("\x00\xff\x10", 200)
	srv := serveBody(t, "image/png", png)
	_, err := b.FetchMarkdown(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary body: err = %v, want a binary-content error", err)
	}
}
