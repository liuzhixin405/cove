package browser

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsPrivateHost_FailClosed is the H-11/fail-open regression: an unresolvable
// host must be blocked, not allowed.
func TestIsPrivateHost_FailClosed(t *testing.T) {
	if !isPrivateHost("nonexistent.invalid.") {
		t.Error("isPrivateHost fails OPEN on DNS failure; unresolvable host must be blocked")
	}
	if !isPrivateHost("127.0.0.1") {
		t.Error("127.0.0.1 must be reported private")
	}
	if isPrivateHost("8.8.8.8") {
		t.Error("8.8.8.8 is public, must not be reported private")
	}
}

// TestBrowserClient_BlocksPrivateDial locks in the redirect/dial SSRF fix: with
// localhost disallowed (the default), the client must refuse a private address
// even when reached directly. This is the mechanism that also defeats a redirect
// (or DNS rebinding) pointing at an internal host.
func TestBrowserClient_BlocksPrivateDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	b := New(DefaultConfig()) // AllowLocalhost=false
	resp, err := b.newHTTPClient().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("browser client reached private address %s; SSRF dial guard failed", srv.URL)
	}
}

// TestBrowserClient_AllowLocalhost verifies the escape hatch still works: when
// localhost is explicitly allowed, the client may reach a local test server.
func TestBrowserClient_AllowLocalhost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.AllowLocalhost = true
	b := New(cfg)
	resp, err := b.newHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("allowLocalhost client should reach %s, got error: %v", srv.URL, err)
	}
	resp.Body.Close()
}
