package tool

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIsPrivateURL covers the SSRF gate. The DNS-failure case is the H-2/fail-open
// regression: before the fix an unresolvable host returned false (allowed), which
// combined with the redirect gap to widen the SSRF surface. It must fail closed.
func TestIsPrivateURL(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1":            true,
		"http://localhost":            true,
		"http://169.254.169.254/x":    true, // cloud metadata
		"http://10.0.0.5":             true,
		"http://192.168.1.1":          true,
		"https://8.8.8.8":             false, // public literal IP
		"http://nonexistent.invalid.": true,  // DNS fails -> must fail CLOSED
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
