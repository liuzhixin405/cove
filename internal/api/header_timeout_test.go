package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A server that accepts the request and never answers is bounded only by the
// transport's ResponseHeaderTimeout (180s cloud, 15 min local), and that
// timeout used to be retried like a refused connection: three more full
// waits, about 12 minutes cloud and an hour local, before the turn failed.
func TestStreamRetriesAResponseHeaderTimeoutOnlyOnce(t *testing.T) {
	fastRetry(t) // MaxRetries 2: the old behaviour made 3 requests
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	tr := &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}
	defer tr.CloseIdleConnections()
	p := &openAICompatProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client(), streamClient: &http.Client{Transport: tr}}
	_, err := p.ChatStream(context.Background(), hiRequest, nil)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d requests, want 2 (one retry of a header timeout)", n)
	}
}
