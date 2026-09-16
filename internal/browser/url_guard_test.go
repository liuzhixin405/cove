package browser

import (
	"net/http"
	"strings"
	"testing"
)

// publicIP is a literal public address, used wherever a test needs a host that
// must PASS the SSRF guard. A literal is deliberate: a hostname would make
// isPrivateHost perform a real DNS lookup, and these tests must never touch the
// network. 8.8.8.8 is never dialed here — only validated.
const publicIP = "8.8.8.8"

// TestValidateURL_SchemeAndHostRules pins browser's own validation contract:
// scheme filtering happens for every caller, private/internal hosts are blocked
// by default, and allowLocalhost relaxes the host check but NOT the scheme
// check. (The private-range predicates themselves belong to internal/safeurl
// and are tested there.)
func TestValidateURL_SchemeAndHostRules(t *testing.T) {
	tests := []struct {
		name           string
		rawURL         string
		allowLocalhost bool
		wantErr        string // substring of the expected error; "" means "must be accepted"
	}{
		{"file scheme rejected", "file:///etc/passwd", false, "unsupported scheme: file"},
		{"gopher scheme rejected", "gopher://example.com/1", false, "unsupported scheme: gopher"},
		{"empty scheme rejected", "example.com/page", false, "unsupported scheme"},
		{"unparseable URL rejected", "http://exa\x7fmple.com/", false, "invalid URL"},
		{"localhost blocked", "http://localhost:8080/admin", false, "private/internal hosts is blocked: localhost"},
		{"loopback v4 blocked", "http://127.0.0.1:9000/", false, "private/internal hosts is blocked: 127.0.0.1"},
		{"loopback v6 blocked", "http://[::1]:3000/", false, "private/internal hosts is blocked: ::1"},
		{"cloud metadata blocked", "http://169.254.169.254/latest/meta-data/", false, "private/internal hosts is blocked: 169.254.169.254"},
		{"public host accepted", "https://" + publicIP + "/search?q=1", false, ""},

		// allowLocalhost is the explicit escape hatch: hosts open up, schemes do not.
		{"localhost allowed when opted in", "http://localhost:8080/admin", true, ""},
		{"loopback v4 allowed when opted in", "http://127.0.0.1:9000/", true, ""},
		{"loopback v6 allowed when opted in", "http://[::1]:3000/", true, ""},
		{"metadata allowed when opted in", "http://169.254.169.254/latest/meta-data/", true, ""},
		{"file scheme still rejected when opted in", "file:///etc/passwd", true, "unsupported scheme: file"},
		{"unparseable still rejected when opted in", "http://exa\x7fmple.com/", true, "invalid URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.AllowLocalhost = tt.allowLocalhost
			err := New(cfg).validateURL(tt.rawURL)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateURL(%q, allowLocalhost=%v) = %v, want accepted", tt.rawURL, tt.allowLocalhost, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateURL(%q, allowLocalhost=%v) accepted the URL, want error containing %q", tt.rawURL, tt.allowLocalhost, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateURL(%q) error = %q, want it to contain %q", tt.rawURL, err.Error(), tt.wantErr)
			}
		})
	}
}

// TestCheckRedirect_ReValidatesEveryHop covers the redirect guard browser relies
// on: the default (localhost-disallowed) client must re-check each hop, so a 3xx
// pointing at the cloud metadata service or an intranet host cannot be followed.
// A nil CheckRedirect would silently follow anything, so its presence is itself
// part of the contract.
func TestCheckRedirect_ReValidatesEveryHop(t *testing.T) {
	client := New(DefaultConfig()).newHTTPClient()
	if client.CheckRedirect == nil {
		t.Fatal("hardened client has no CheckRedirect: redirect hops would never be re-validated")
	}

	newReq := func(rawURL string) *http.Request {
		t.Helper()
		req, err := http.NewRequest("GET", rawURL, nil)
		if err != nil {
			t.Fatalf("build request %q: %v", rawURL, err)
		}
		return req
	}
	firstHop := []*http.Request{newReq("https://" + publicIP + "/start")}

	blocked := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:9000/",
		"http://[::1]:9000/",
		"http://localhost/admin",
		"http://10.0.0.5/internal",
	}
	for _, target := range blocked {
		err := client.CheckRedirect(newReq(target), firstHop)
		if err == nil {
			t.Errorf("CheckRedirect allowed a redirect to %s; SSRF guard is not re-validating hops", target)
			continue
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("CheckRedirect(%s) error = %q, want it to say the hop was blocked", target, err.Error())
		}
	}

	if err := client.CheckRedirect(newReq("https://"+publicIP+"/next"), firstHop); err != nil {
		t.Errorf("CheckRedirect refused a public redirect target: %v", err)
	}
}

// TestCheckRedirect_StopsLongChain asserts the hop counter actually terminates a
// redirect loop instead of following it forever.
func TestCheckRedirect_StopsLongChain(t *testing.T) {
	client := New(DefaultConfig()).newHTTPClient()
	if client.CheckRedirect == nil {
		t.Fatal("hardened client has no CheckRedirect")
	}
	req, err := http.NewRequest("GET", "https://"+publicIP+"/loop", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	chain := make([]*http.Request, 0, 10)
	for i := 0; i < 9; i++ {
		chain = append(chain, req)
	}
	if err := client.CheckRedirect(req, chain); err != nil {
		t.Fatalf("CheckRedirect stopped at hop %d, too early: %v", len(chain), err)
	}

	chain = append(chain, req) // 10 hops already made
	err = client.CheckRedirect(req, chain)
	if err == nil {
		t.Fatal("CheckRedirect allowed an 11th hop: an endless redirect chain would never terminate")
	}
	if !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("CheckRedirect error = %q, want it to mention the redirect limit", err.Error())
	}
}

// TestNewConfigDefaults locks in the defaulting New() performs, because a zero
// or negative value must not turn into "no timeout" or "read the whole body".
func TestNewConfigDefaults(t *testing.T) {
	b := New(Config{Timeout: 0, MaxBodySize: 0})
	if b.timeout != DefaultConfig().Timeout {
		t.Errorf("timeout = %v, want %v", b.timeout, DefaultConfig().Timeout)
	}
	if b.maxBodySize != DefaultConfig().MaxBodySize {
		t.Errorf("maxBodySize = %d, want %d", b.maxBodySize, DefaultConfig().MaxBodySize)
	}

	b = New(Config{Timeout: -1, MaxBodySize: -1})
	if b.timeout <= 0 || b.maxBodySize <= 0 {
		t.Errorf("negative config not defaulted: timeout=%v maxBodySize=%d", b.timeout, b.maxBodySize)
	}

	b = New(Config{Timeout: 42, MaxBodySize: 4096, AllowLocalhost: true})
	if b.timeout != 42 || b.maxBodySize != 4096 || !b.allowLocalhost {
		t.Errorf("explicit config not preserved: %+v", *b)
	}
}
