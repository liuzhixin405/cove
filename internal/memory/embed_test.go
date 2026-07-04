package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoteAPIEmbeddingProvider_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		inputs, _ := req["input"].([]any)
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			data[i] = map[string]any{"embedding": []float32{1, 0, 0}, "index": i}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	p := NewRemoteAPIEmbeddingProvider(srv.URL, "sk-test", "")
	vecs, err := p.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 || len(vecs[1]) != 3 {
		t.Fatalf("unexpected result: %+v", vecs)
	}
	if p.Dim() != 3 {
		t.Fatalf("expected Dim() updated to 3 from the real response, got %d", p.Dim())
	}
}

func TestRemoteAPIEmbeddingProvider_NoAPIKey(t *testing.T) {
	p := NewRemoteAPIEmbeddingProvider("http://127.0.0.1:1", "", "")
	if _, err := p.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected an error when no API key is configured")
	}
}

func TestRemoteAPIEmbeddingProvider_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()

	p := NewRemoteAPIEmbeddingProvider(srv.URL, "sk-bad", "")
	_, err := p.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected a 401 error, got: %v", err)
	}
}

func TestRemoteAPIEmbeddingProvider_EmptyInput(t *testing.T) {
	p := NewRemoteAPIEmbeddingProvider("http://127.0.0.1:1", "sk-test", "")
	vecs, err := p.Embed(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Fatalf("expected (nil, nil) for empty input, got (%v, %v)", vecs, err)
	}
}

func TestRemoteAPIEmbeddingProvider_MissingEmbeddingInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only returns an embedding for index 0, silently drops index 1 —
		// must be treated as an error, not a partial/nil result.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{1, 0}, "index": 0},
			},
		})
	}))
	defer srv.Close()

	p := NewRemoteAPIEmbeddingProvider(srv.URL, "sk-test", "")
	if _, err := p.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected an error when the response is missing an embedding for one input")
	}
}
