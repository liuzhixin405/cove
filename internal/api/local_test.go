package api

import (
	"net/http"
	"testing"
	"time"
)

func TestIsLocalBaseURL(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8080/v1":          true,
		"http://localhost:1234/v1":          true,
		"http://[::1]:8080":                 true,
		"http://host.docker.internal:11434": true,
		"https://api.deepseek.com/v1":       false,
		"https://api.openai.com/v1":         false,
		"":                                  false,
	}
	for u, want := range cases {
		if got := IsLocalBaseURL(u); got != want {
			t.Errorf("IsLocalBaseURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// A local server prefilling a 16K prompt and writing a summary can take
// longer than three minutes; the compaction summary call timed out at the
// cloud-sized 180s and the history was truncated instead. Local providers
// get a client and transport that wait for them; remote ones keep the
// cloud timeouts.
func TestLocalProvidersGetLongerTimeouts(t *testing.T) {
	local := newOpenAICompatProvider(ProviderConfig{Name: "openai-compatible", APIKey: "local", BaseURL: "http://127.0.0.1:8080/v1"})
	if local.client.Timeout < 10*time.Minute {
		t.Fatalf("local non-stream client timeout = %v, want at least 10 minutes", local.client.Timeout)
	}
	if tr, ok := local.streamClient.Transport.(*http.Transport); !ok || tr.ResponseHeaderTimeout < 10*time.Minute {
		t.Fatalf("local stream transport header timeout = %v, want at least 10 minutes", headerTimeout(local.streamClient))
	}
	remote := newOpenAICompatProvider(ProviderConfig{Name: "deepseek", APIKey: "k", BaseURL: "https://api.deepseek.com/v1"})
	if remote.client.Timeout != 180*time.Second {
		t.Fatalf("remote client timeout changed to %v", remote.client.Timeout)
	}
	if headerTimeout(remote.streamClient) != 180*time.Second {
		t.Fatalf("remote header timeout changed to %v", headerTimeout(remote.streamClient))
	}
}

func headerTimeout(c *http.Client) time.Duration {
	if tr, ok := c.Transport.(*http.Transport); ok {
		return tr.ResponseHeaderTimeout
	}
	return 0
}
