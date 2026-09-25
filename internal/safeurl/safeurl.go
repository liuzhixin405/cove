// Package safeurl centralizes the SSRF guards used by every outbound HTTP
// fetch in cove.
//
// The controls here are not something each caller can reasonably re-derive:
// the guard has to reject private/loopback/link-local targets, re-validate
// every redirect hop (a 3xx to 169.254.169.254 otherwise walks straight into
// cloud metadata), and pin the connection to an address it has already vetted
// so DNS cannot be rebound between the check and the dial. Two near-identical
// copies of exactly this logic had grown in internal/tool and internal/browser
// while a third caller (the skills registry fetch) had no guard at all, which
// is the failure mode this package exists to prevent.
package safeurl

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxRedirects bounds a redirect chain. Every hop is re-validated.
const maxRedirects = 10

// privateCIDRs are the ranges that must never be reachable from a fetch.
var privateCIDRs = []string{
	"127.0.0.0/8",                                   // loopback
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // RFC1918
	"169.254.0.0/16", // link-local, incl. 169.254.169.254 cloud metadata
	"100.64.0.0/10",  // RFC6598 carrier NAT
	"192.0.0.0/24",   // IETF protocol assignments
	"0.0.0.0/8",      // "this network"; 0.0.0.0 reaches the local host on Linux/macOS
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, incl. 255.255.255.255 broadcast
	"::1/128",        // IPv6 loopback
	"fc00::/7",       // IPv6 unique-local
	"fe80::/10",      // IPv6 link-local
	"ff00::/8",       // IPv6 multicast
	"64:ff9b::/96",   // NAT64: embeds an IPv4 address (64:ff9b::a00:1 is 10.0.0.1)
}

// literalOnlyCIDRs are refused when a URL names them as a literal address, but
// not when a hostname resolves into them.
//
// 198.18.0.0/15 is the benchmarking range, and it is also where fake-IP DNS
// (Clash/Surge TUN mode, common on machines behind the GFW) puts every name
// it answers, proxying the connection to the real host. Refusing resolved
// addresses in it would refuse every fetch on such a machine, while a target
// someone spells out as http://198.18.x.y/ has no business being fetched.
var literalOnlyCIDRs = []string{
	"198.18.0.0/15",
}

// proxyFromEnvironment picks the proxy for a request. It is a variable so tests
// can supply a proxy: http.ProxyFromEnvironment reads the environment once per
// process and caches it.
var proxyFromEnvironment = http.ProxyFromEnvironment

// lookupIP resolves a hostname for IsPrivateHost. A variable so tests do not
// depend on the machine's resolver: fake-IP DNS (Clash/Surge TUN mode) answers
// every name, even "x.invalid.", with an address in 198.18.0.0/15.
var lookupIP = net.LookupIP

var parsedPrivate, parsedLiteralOnly []*net.IPNet

func init() {
	parsedPrivate = mustParseCIDRs(privateCIDRs)
	parsedLiteralOnly = mustParseCIDRs(literalOnlyCIDRs)
}

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("safeurl: bad CIDR " + cidr)
		}
		out = append(out, n)
	}
	return out
}

// isPrivateLiteral is IsPrivateIP for an address the URL (or dial target)
// names literally, which additionally covers literalOnlyCIDRs.
func isPrivateLiteral(ip net.IP) bool {
	if IsPrivateIP(ip) {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range parsedLiteralOnly {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IsPrivateIP reports whether ip must not be reachable from a fetch.
// A nil IP is treated as private (fail closed).
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// An IPv4-mapped IPv6 address must be judged on its IPv4 value.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range parsedPrivate {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IsPrivateHost reports whether host (a hostname or literal IP) resolves to
// anything that must not be reachable.
//
// It FAILS CLOSED: a host that cannot be resolved is reported as private,
// because an unresolvable name is not proof that the target is public. Every
// resolved address is checked, not just the first — a hostname can return a
// mix of public and private records.
func IsPrivateHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return true
	}
	// Strip an IPv6 literal's brackets.
	host = strings.Trim(host, "[]")
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		lower == "metadata.google.internal" || strings.HasSuffix(lower, ".internal") ||
		strings.HasSuffix(lower, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateLiteral(ip)
	}
	ips, err := lookupIP(host)
	if err != nil || len(ips) == 0 {
		return true
	}
	for _, ip := range ips {
		if IsPrivateIP(ip) {
			return true
		}
	}
	return false
}

// IsPrivateURL reports whether rawURL points at something that must not be
// fetched. A URL that cannot be parsed, or that uses a scheme other than
// http/https, is reported as private (fail closed).
func IsPrivateURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return true
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		// file://, gopher://, ftp:// and friends are never legitimate here.
		return true
	}
	return IsPrivateHost(u.Hostname())
}

// ValidateURL returns an error describing why rawURL may not be fetched, or nil
// when it is acceptable.
func ValidateURL(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("empty URL")
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported URL scheme %q (only http/https)", u.Scheme)
	}
	if IsPrivateHost(u.Hostname()) {
		return fmt.Errorf("blocked: %s is private, internal or unresolvable", u.Hostname())
	}
	return nil
}

// NewClient returns an HTTP client hardened against SSRF.
//
// timeout bounds the whole request, so do NOT use this client for streaming
// responses. Pass 0 to leave Client.Timeout unset (header and handshake phases
// are still bounded by the transport).
func NewClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 10 * time.Second}

	// The transport dials a configured proxy through safeDial too. A proxy
	// normally lives on loopback or the LAN (HTTPS_PROXY=http://127.0.0.1:7890,
	// a corporate 10.x proxy), so every fetch used to be refused at the proxy
	// dial. The proxy comes from the user's own environment, not from the
	// fetched URL, so its address is allowed; the target is then vetted in
	// proxy() instead, because the proxy — not safeDial — connects to it.
	var proxyAddrs sync.Map // canonical "host:port" of proxies in use
	proxy := func(req *http.Request) (*url.URL, error) {
		p, err := proxyFromEnvironment(req)
		if err != nil || p == nil {
			return p, err
		}
		if IsPrivateHost(req.URL.Hostname()) {
			return nil, fmt.Errorf("blocked: %s is private, internal or unresolvable", req.URL.Hostname())
		}
		proxyAddrs.Store(canonicalProxyAddr(p), true)
		return p, nil
	}

	safeDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, ok := proxyAddrs.Load(addr); ok {
			return dialer.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip != nil && isPrivateLiteral(ip) {
			return nil, fmt.Errorf("blocked: %s is a private address", host)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("blocked: cannot resolve host %q", host)
		}
		for _, ip := range ips {
			if IsPrivateIP(ip) {
				return nil, fmt.Errorf("blocked: %s resolves to private address %s", host, ip)
			}
		}
		// Dial an address that was just vetted, rather than the hostname, so the
		// name cannot be rebound to a private IP between check and connect.
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}

	headerTimeout := timeout
	if headerTimeout <= 0 {
		headerTimeout = 60 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 proxy,
			DialContext:           safeDial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: headerTimeout,
			MaxIdleConns:          10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// Re-validate every hop: only the initial URL was ever checked by
			// the caller, and one redirect is enough to reach the metadata
			// service or an intranet host.
			if IsPrivateURL(req.URL.String()) {
				return fmt.Errorf("blocked: redirect to private/internal URL %s", req.URL)
			}
			return nil
		},
	}
}

// canonicalProxyAddr returns the "host:port" the transport dials for proxy p,
// filling in the scheme's default port the way net/http does.
func canonicalProxyAddr(p *url.URL) string {
	port := p.Port()
	if port == "" {
		switch strings.ToLower(p.Scheme) {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(p.Hostname(), port)
}
