package safeurl

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestClientDialsConfiguredLocalProxy covers users behind a local proxy
// (HTTPS_PROXY=http://127.0.0.1:7890 is the usual Clash/v2ray setup). The
// transport dials the proxy through the guarded dialer, and the proxy's own
// address is loopback, so every fetch used to fail with "blocked: resolves to
// private address" even though the target itself was public.
func TestClientDialsConfiguredLocalProxy(t *testing.T) {
	var gotURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = w.Write([]byte("via proxy"))
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	old := proxyFromEnvironment
	proxyFromEnvironment = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	defer func() { proxyFromEnvironment = old }()

	// 8.8.8.8 is a public literal: nothing is resolved and, with the proxy in
	// place, nothing but the local proxy is ever dialed.
	resp, err := NewClient(5 * time.Second).Get("http://8.8.8.8/x")
	if err != nil {
		t.Fatalf("request through a configured loopback proxy failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via proxy" || !strings.Contains(gotURL, "8.8.8.8/x") {
		t.Fatalf("proxy got %q, body %q", gotURL, body)
	}
}

// TestClientBlocksPrivateTargetThroughProxy: once a proxy is in use, the
// proxy (not safeDial) connects to the target, so the target has to be vetted
// before the request is handed to it — otherwise any direct caller could reach
// the intranet through the user's proxy.
func TestClientBlocksPrivateTargetThroughProxy(t *testing.T) {
	hit := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte("intranet"))
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	old := proxyFromEnvironment
	proxyFromEnvironment = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	defer func() { proxyFromEnvironment = old }()

	_, err := NewClient(5 * time.Second).Get("http://10.1.2.3/admin")
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected the private target to be blocked, got %v", err)
	}
	if hit {
		t.Fatal("the request for a private target reached the proxy")
	}
}

// TestClientStillBlocksPrivateTargetWithoutProxy makes sure the proxy
// exemption is scoped to the proxy address: with no proxy configured, a
// loopback target is still refused at dial time.
func TestClientStillBlocksPrivateTargetWithoutProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	old := proxyFromEnvironment
	proxyFromEnvironment = func(*http.Request) (*url.URL, error) { return nil, nil }
	defer func() { proxyFromEnvironment = old }()

	if _, err := NewClient(5 * time.Second).Get(srv.URL); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected a blocked dial to loopback, got %v", err)
	}
}
