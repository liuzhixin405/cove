package safeurl

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":        true,
		"127.1.2.3":        true,
		"10.0.0.1":         true,
		"172.16.0.1":       true,
		"172.31.255.255":   true,
		"192.168.1.1":      true,
		"169.254.169.254":  true, // cloud metadata
		"100.64.0.1":       true, // RFC6598 carrier NAT
		"0.0.0.0":          true,
		"::1":              true,
		"fc00::1":          true,
		"fe80::1":          true,
		"::ffff:127.0.0.1": true, // IPv4-mapped loopback
		"::ffff:10.0.0.1":  true, // IPv4-mapped RFC1918

		"8.8.8.8":         false,
		"1.1.1.1":         false,
		"172.32.0.1":      false, // just outside 172.16/12
		"93.184.216.34":   false,
		"2606:4700::1111": false,
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test fixture %q is not an IP", s)
		}
		if got := IsPrivateIP(ip); got != want {
			t.Errorf("IsPrivateIP(%s) = %v, want %v", s, got, want)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("IsPrivateIP(nil) must fail closed")
	}
}

func TestIsPrivateHostFailsClosed(t *testing.T) {
	// An unresolvable name is not proof the target is public. The resolver is
	// stubbed: this used to ask the real one, and fake-IP DNS (Clash TUN mode,
	// common behind the GFW) resolves even "x.invalid." to 198.18.0.0/15, so
	// the test failed on those machines without the code being wrong.
	old := lookupIP
	lookupIP = func(string) ([]net.IP, error) { return nil, &net.DNSError{Err: "no such host", IsNotFound: true} }
	defer func() { lookupIP = old }()
	if !IsPrivateHost("nonexistent.invalid.") {
		t.Error("an unresolvable host must be treated as private")
	}
	if !IsPrivateHost("") {
		t.Error("an empty host must be treated as private")
	}
	for _, h := range []string{
		"localhost", "LOCALHOST", "foo.localhost",
		"metadata.google.internal", "svc.internal", "printer.local",
		"127.0.0.1", "[::1]", "169.254.169.254",
	} {
		if !IsPrivateHost(h) {
			t.Errorf("IsPrivateHost(%q) = false, want true", h)
		}
	}
}

func TestIsPrivateURLRejectsNonHTTPSchemes(t *testing.T) {
	for _, u := range []string{
		"file:///etc/passwd",
		"gopher://evil/x",
		"ftp://evil/x",
		"dict://127.0.0.1:11211/",
		"",
		"http://169.254.169.254/latest/meta-data/",
		"https://localhost/admin",
	} {
		if !IsPrivateURL(u) {
			t.Errorf("IsPrivateURL(%q) = false, want true", u)
		}
	}
}

func TestValidateURL(t *testing.T) {
	if err := ValidateURL("file:///etc/passwd"); err == nil {
		t.Error("file:// accepted")
	} else if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("unexpected error for file://: %v", err)
	}
	if err := ValidateURL("http://127.0.0.1/x"); err == nil {
		t.Error("loopback accepted")
	}
	if err := ValidateURL(""); err == nil {
		t.Error("empty URL accepted")
	}
	if err := ValidateURL("https://example.com/x"); err != nil {
		t.Errorf("a public https URL was rejected: %v", err)
	}
}

// TestClientBlocksPrivateDial asserts the dial-time check, which is what closes
// the DNS-rebinding window: even a hostname that passed an earlier check cannot
// be connected to if it resolves to a private address now.
func TestClientBlocksPrivateDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	// httptest listens on 127.0.0.1, so the dial must be refused.
	_, err := NewClient(5 * time.Second).Get(srv.URL)
	if err == nil {
		t.Fatal("the client connected to a loopback address")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected a blocked-dial error, got: %v", err)
	}
}

// TestClientRevalidatesRedirects covers the redirect hop check: only the initial
// URL is ever vetted by the caller, and one 302 is enough to reach an internal
// address.
func TestClientRevalidatesRedirects(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("internal secret"))
	}))
	defer internal.Close()

	// A CheckRedirect that refuses the hop is what we are asserting; the client
	// cannot reach the public hop in a unit test, so exercise CheckRedirect
	// directly with a request aimed at the loopback server.
	c := NewClient(5 * time.Second)
	req, _ := http.NewRequest("GET", internal.URL, nil)
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Fatal("CheckRedirect allowed a redirect to a loopback address")
	}

	// And it must stop a redirect loop even for public targets.
	pub, _ := http.NewRequest("GET", "https://example.com/", nil)
	via := make([]*http.Request, maxRedirects)
	if err := c.CheckRedirect(pub, via); err == nil {
		t.Fatal("CheckRedirect did not stop an over-long redirect chain")
	}
}

// Ranges that were missing: "this network", NAT64 (which embeds an IPv4
// address, here 10.0.0.1), benchmarking, multicast, reserved, IPv6 multicast.
func TestIsPrivateURLCoversSpecialPurposeRanges(t *testing.T) {
	for _, u := range []string{
		"http://0.0.0.0/",
		"http://0.1.2.3/",
		"http://[64:ff9b::a00:1]/",
		"http://198.18.0.1/",
		"http://198.19.255.254/",
		"http://224.0.0.1/",
		"http://239.255.255.250/",
		"http://240.0.0.1/",
		"http://255.255.255.255/",
		"http://[ff02::1]/",
	} {
		if !IsPrivateURL(u) {
			t.Errorf("IsPrivateURL(%q) = false, want true", u)
		}
	}
	for _, u := range []string{"http://8.8.8.8/", "http://198.20.0.1/", "http://223.255.255.1/", "http://[2606:4700::1111]/"} {
		if IsPrivateURL(u) {
			t.Errorf("IsPrivateURL(%q) = true, want false", u)
		}
	}
}

// Fake-IP DNS (Clash/Surge TUN mode) answers every name with an address in
// 198.18.0.0/15 and proxies the connection to the real host. Blocking those
// resolved addresses would block every fetch on such a machine, so the range
// is refused only when the URL names it literally.
func TestFakeIPResolvedAddressIsNotBlocked(t *testing.T) {
	old := lookupIP
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("198.18.0.7")}, nil }
	defer func() { lookupIP = old }()
	if IsPrivateHost("example.com") {
		t.Error("a hostname resolved by fake-IP DNS was treated as private")
	}
	if !IsPrivateHost("198.18.0.7") {
		t.Error("a literal 198.18.0.0/15 address was allowed")
	}
}
