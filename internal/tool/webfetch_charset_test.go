package tool

import (
	"strings"
	"testing"
)

// A scheme-less URL that happens to start with "http" (httpbin.org) used to be
// left alone and failed with unsupported protocol scheme "".
func TestNormalizeFetchURL(t *testing.T) {
	cases := map[string]string{
		"httpbin.org/get":           "https://httpbin.org/get",
		"example.com":               "https://example.com",
		"http://example.com/a":      "https://example.com/a",
		"https://example.com/a":     "https://example.com/a",
		"HTTP://Example.com/":       "https://Example.com/",
		"  https://example.com  ":   "https://example.com",
		"http-docs.example.org/x":   "https://http-docs.example.org/x",
		"example.com/?next=http://": "https://example.com/?next=http://",
	}
	for in, want := range cases {
		if got := normalizeFetchURL(in); got != want {
			t.Errorf("normalizeFetchURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// Content-Type charset and <meta charset> were ignored, so GBK pages came
// back garbled.
func TestFetchedTextDecodesDeclaredCharset(t *testing.T) {
	page := mustGBK(t, "<html><head><title>中文</title></head><body><p>你好，世界</p></body></html>")
	got, err := fetchedText(page, "text/html; charset=GBK", "text")
	if err != nil || !strings.Contains(got, "你好，世界") {
		t.Fatalf("header charset: %q, %v", got, err)
	}
	meta := append([]byte(`<html><head><meta http-equiv="Content-Type" content="text/html; charset=gb2312"></head><body>`),
		mustGBK(t, "<p>简体中文页面</p></body></html>")...)
	got, err = fetchedText(meta, "text/html", "markdown")
	if err != nil || !strings.Contains(got, "简体中文页面") {
		t.Fatalf("meta charset: %q, %v", got, err)
	}
	// Undeclared, but plainly GBK.
	got, err = fetchedText(mustGBK(t, "纯文本，没有声明编码\n"), "text/plain", "text")
	if err != nil || !strings.Contains(got, "纯文本") {
		t.Fatalf("undeclared GBK: %q, %v", got, err)
	}
	// UTF-8 stays as it is.
	got, err = fetchedText([]byte("<p>已经是 UTF-8</p>"), "text/html; charset=utf-8", "text")
	if err != nil || got != "已经是 UTF-8" {
		t.Fatalf("utf-8: %q, %v", got, err)
	}
}
