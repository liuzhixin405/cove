package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// EmbeddingProvider returns vector embeddings for given texts.
// Implementations may use an external API (OpenAI, Anthropic) or a local model.
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

// RemoteAPIEmbeddingProvider calls a real OpenAI-compatible /embeddings
// endpoint. It is designed to reuse the base URL and API key the user
// already configured for chat completions — no separate account, no local
// model download — so enabling it is just pointing at an existing,
// already-cheap embeddings endpoint (most OpenAI-compatible providers,
// including DeepSeek/GLM/Qwen-family ones, price embeddings far below their
// chat models). See docs/中等模型平替优化建议.md §2.2 for the reasoning
// behind not requiring a locally-installed model.
//
// Every failure mode (unreachable endpoint, non-200 response, malformed
// body) is returned as a plain Go error rather than panicking or silently
// substituting garbage data — Store treats any such error as "vector search
// unavailable this round, fall back to BM25 only" (see store.go).
type RemoteAPIEmbeddingProvider struct {
	baseURL string
	apiKey  string
	model   string
	dim     int // best-known embedding dimension; updated from the first real response
	client  *http.Client
}

// NewRemoteAPIEmbeddingProvider creates a provider that calls
// baseURL+"/embeddings" using the given API key and model. If model is
// empty, "text-embedding-3-small" is used (widely supported by OpenAI and
// most OpenAI-compatible providers).
func NewRemoteAPIEmbeddingProvider(baseURL, apiKey, model string) *RemoteAPIEmbeddingProvider {
	if model == "" {
		model = "text-embedding-3-small"
	}
	return &RemoteAPIEmbeddingProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		dim:     1536, // placeholder until the first real response updates it
		client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *RemoteAPIEmbeddingProvider) Dim() int { return p.dim }

type remoteEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type remoteEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

func (p *RemoteAPIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if p.apiKey == "" {
		return nil, fmt.Errorf("remote embeddings: no API key configured")
	}

	body, err := json.Marshal(remoteEmbedRequest{Model: p.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("remote embeddings: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("remote embeddings: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("remote embeddings: request failed: %w", err)
	}
	defer httpResp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10*1024*1024))
	if httpResp.StatusCode != 200 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("remote embeddings: API error %d: %s", httpResp.StatusCode, msg)
	}

	var parsed remoteEmbedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("remote embeddings: decode response: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("remote embeddings: API returned no embeddings")
	}

	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	for _, v := range out {
		if v == nil {
			return nil, fmt.Errorf("remote embeddings: API response missing an embedding for one or more inputs")
		}
	}
	if len(parsed.Data[0].Embedding) > 0 {
		p.dim = len(parsed.Data[0].Embedding)
	}
	return out, nil
}

// cosineSimilarity returns the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		da := float64(a[i])
		db := float64(b[i])
		dot += da * db
		normA += da * da
		normB += db * db
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
