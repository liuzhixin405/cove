package browser

import "testing"

// shouldBlockRequest is what the headless browser asks for every request the
// page makes (subresources, redirects, fetch/XHR), not only the URL cove was
// told to open.
func TestShouldBlockRequest(t *testing.T) {
	for _, u := range []string{
		"http://0.0.0.0/",
		"http://[64:ff9b::a00:1]/",
		"http://198.18.0.1/",
		"http://224.0.0.1/",
		"http://127.0.0.1:9222/json",
		"http://169.254.169.254/latest/meta-data/",
		"https://10.1.2.3/",
		"ws://127.0.0.1:8080/socket",
		"file:///etc/passwd",
		"chrome://settings",
		"ftp://example.com/",
		"",
		"://bad",
	} {
		if !shouldBlockRequest(u) {
			t.Errorf("shouldBlockRequest(%q) = false, want true", u)
		}
	}
	for _, u := range []string{
		"https://" + publicIP + "/",
		"http://" + publicIP + ":8080/x.js",
		"wss://" + publicIP + "/live",
		"data:image/png;base64,iVBORw0KGgo=",
		"blob:https://example.com/0f5d",
		"about:blank",
	} {
		if shouldBlockRequest(u) {
			t.Errorf("shouldBlockRequest(%q) = true, want false", u)
		}
	}
}
