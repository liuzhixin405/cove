package api

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// IsLocalBaseURL reports whether baseURL points at a server on this machine
// or the host's own network (a loopback address, host.docker.internal): a
// llama.cpp, LM Studio, Ollama or vLLM the user runs. Such a server has no
// rate limits and no quotas, but it can take minutes to prefill a long
// prompt before its first byte, so it gets longer timeouts and a slower
// stall clock.
func IsLocalBaseURL(baseURL string) bool {
	u := strings.ToLower(strings.TrimSpace(baseURL))
	if u == "" {
		return false
	}
	return strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1") ||
		strings.Contains(u, "[::1]") || strings.Contains(u, "0.0.0.0") || strings.Contains(u, "host.docker.internal")
}

// Timeouts for a local server. The non-stream client timeout bounds a
// whole request (compaction summaries, tool-free calls); the header timeout
// bounds the wait for the first byte of a stream, which is the prefill.
const (
	localClientTimeout         = 20 * time.Minute
	localResponseHeaderTimeout = 15 * time.Minute
)

var (
	localTransportOnce sync.Once
	localTransport     *http.Transport
)

// localHTTPTransport is defaultHTTPTransport with the response header
// timeout a local server needs. It is a separate transport, so the cloud
// providers' connections keep the cloud limits.
func localHTTPTransport() *http.Transport {
	localTransportOnce.Do(func() {
		t := defaultHTTPTransport().Clone()
		t.ResponseHeaderTimeout = localResponseHeaderTimeout
		localTransport = t
	})
	return localTransport
}
