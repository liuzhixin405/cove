package browser

import (
	"net/url"
	"strings"

	"github.com/liuzhixin405/cove/internal/safeurl"
)

// shouldBlockRequest reports whether headless Chrome must refuse a request the
// page makes. It is applied to every request, not only to the URL cove opens:
// validating the start URL alone let a public page load, redirect to, or
// fetch() http://127.0.0.1:…, the cloud metadata service or an intranet host,
// and Chrome resolves and connects on its own, past safeurl's HTTP client.
//
// Schemes that never reach the network (data:, blob:, about:) pass; http(s)
// and ws(s) pass only for a public host; anything else (file:, chrome:, ftp:,
// an unparsable URL) is refused.
func shouldBlockRequest(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" {
		return true
	}
	switch strings.ToLower(u.Scheme) {
	case "data", "blob", "about":
		return false
	case "http", "https":
		return safeurl.IsPrivateURL(rawURL)
	case "ws", "wss":
		return safeurl.IsPrivateHost(u.Hostname())
	default:
		return true
	}
}
