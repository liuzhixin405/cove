package tool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIsPrivateURL covers the SSRF gate. The DNS-failure case (an unresolvable
// host must fail closed) is tested in internal/safeurl with a stubbed
// resolver: resolving a real name here failed on machines whose DNS answers
// every name (fake-IP proxies resolve nonexistent.invalid. to 198.18.0.0/15).
func TestIsPrivateURL(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1":         true,
		"http://localhost":         true,
		"http://169.254.169.254/x": true, // cloud metadata
		"http://10.0.0.5":          true,
		"http://192.168.1.1":       true,
		"https://8.8.8.8":          false, // public literal IP
	}
	for u, want := range cases {
		if got := isPrivateURL(u); got != want {
			t.Errorf("isPrivateURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// TestSafeHTTPClient_BlocksPrivateDial locks in H-1/H-11: the hardened client must
// refuse to connect to a private address even when reached directly. httptest
// servers listen on 127.0.0.1, so a successful GET would mean the dial-time IP
// guard failed (this is also the mechanism that defeats DNS rebinding on redirect).
func TestSafeHTTPClient_BlocksPrivateDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	client := newSafeHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL) // srv.URL is http://127.0.0.1:PORT
	if err == nil {
		resp.Body.Close()
		t.Fatalf("safe client reached private address %s; SSRF dial guard failed", srv.URL)
	}
}

func TestFetchedBodyRefusesBinary(t *testing.T) {
	pdf := append([]byte("%PDF-1.7\n"), make([]byte, 64)...)
	if _, err := fetchedText(pdf, "application/pdf", "markdown"); err == nil {
		t.Fatal("binary body was returned as text")
	}
	got, err := fetchedText([]byte("<html><body><h1>Title</h1><p>hi</p></body></html>"), "text/html; charset=utf-8", "markdown")
	if err != nil || !strings.Contains(got, "# Title") {
		t.Fatalf("html body = %q, %v", got, err)
	}
}
