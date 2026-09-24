package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
)

// The shared transport had no Proxy func, so HTTPS_PROXY / HTTP_PROXY were
// ignored and users behind a corporate or regional proxy could not reach the
// API at all, while every other tool on the machine worked.
//
// net/http reads the proxy variables once per process, so the request runs
// in a child test process that starts with them set.
func TestProviderRequestsHonorProxyEnvironment(t *testing.T) {
	if os.Getenv("COVE_PROXY_CHILD") == "1" {
		p := newOpenAICompatProvider(ProviderConfig{Name: "deepseek", APIKey: "k", BaseURL: "http://cove-proxy-test.invalid/v1"})
		resp, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-pro", Messages: []Message{{Role: "user", Content: "hi"}}})
		if err != nil {
			t.Fatalf("request did not go through the proxy: %v", err)
		}
		if resp.Content != "via proxy" {
			t.Fatalf("content = %q", resp.Content)
		}
		return
	}

	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "cove-proxy-test.invalid" {
			proxied.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"via proxy"}}]}`)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestProviderRequestsHonorProxyEnvironment$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"COVE_PROXY_CHILD=1",
		"HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL,
		"HTTPS_PROXY="+proxy.URL, "https_proxy="+proxy.URL,
		"NO_PROXY=", "no_proxy=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child test failed: %v\n%s", err, out)
	}
	if proxied.Load() == 0 {
		t.Fatalf("the proxy never saw the request\n%s", out)
	}
}
