package memory

import (
	"context"
	"encoding/json"
	"math"
	"strings"
)

// EmbeddingProvider returns vector embeddings for given texts.
// Implementations may use an external API (OpenAI, Anthropic) or a local model.
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

// APIEmbeddingProvider uses pseudo-embeddings (character n-gram based).
// provider and model are stored for future integration with real embedding APIs.
type APIEmbeddingProvider struct {
	dim int
}

// NewAPIEmbeddingProvider creates a provider that uses pseudo-embeddings
// (character n-gram based) rather than calling the LLM's embedding API.
// This avoids additional API costs for the memory layer.
func NewAPIEmbeddingProvider(dim int) *APIEmbeddingProvider {
	if dim <= 0 {
		dim = 384
	}
	return &APIEmbeddingProvider{dim: dim}
}

func (p *APIEmbeddingProvider) Dim() int { return p.dim }

func (p *APIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	for i, text := range texts {
		results[i] = pseudoEmbedding(text, p.dim)
	}
	return results, nil
}

// pseudoEmbedding creates a deterministic pseudo-embedding from text.
// This is a fallback when real embeddings are unavailable.
// It's based on character n-grams hashed to float32 values.
func pseudoEmbedding(text string, dim int) []float32 {
	vec := make([]float32, dim)

	// Simple bag-of-character-ngrams approach
	lower := strings.ToLower(text)
	for i := 0; i < len(lower)-2; i++ {
		trigram := lower[i : i+3]
		hash := hashTrigram(trigram)
		idx := int(hash % uint32(dim))
		vec[idx] += 1.0
	}

	// Normalize
	var sum float64
	for _, v := range vec {
		sum += float64(v * v)
	}
	if sum > 0 {
		norm := float32(math.Sqrt(sum))
		for i := range vec {
			vec[i] /= norm
		}
	}
	return vec
}

func hashTrigram(s string) uint32 {
	var h uint32
	for _, c := range []byte(s) {
		h = h*31 + uint32(c)
	}
	return h
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

// SearchResult is a single match from the vector store.
type SearchResult struct {
	MemoryName  string
	ChunkIndex  int
	ChunkText   string
	Score       float64
}

// VectorStore is an in-memory vector index for memory chunks.
// It uses brute-force cosine similarity (suitable for ≤1000 chunks).
type VectorStore struct {
	dim      int
	entries  []vectorEntry
}

type vectorEntry struct {
	memoryName string
	chunkIndex int
	chunkText  string
	embedding  []float32
}

// NewVectorStore creates an empty vector store.
func NewVectorStore(dim int) *VectorStore {
	return &VectorStore{
		dim:     dim,
		entries: make([]vectorEntry, 0),
	}
}

// Clear removes all entries.
func (vs *VectorStore) Clear() {
	vs.entries = nil
}

// Add stores a chunk with its embedding.
func (vs *VectorStore) Add(memoryName string, chunkIndex int, chunkText string, embedding []float32) {
	vs.entries = append(vs.entries, vectorEntry{
		memoryName: memoryName,
		chunkIndex: chunkIndex,
		chunkText:  chunkText,
		embedding:  embedding,
	})
}

// Search finds the top-K most similar chunks to the query embedding.
func (vs *VectorStore) Search(queryVec []float32, topK int) []SearchResult {
	type scored struct {
		entry vectorEntry
		score float64
	}

	var all []scored
	for _, e := range vs.entries {
		sim := cosineSimilarity(queryVec, e.embedding)
		all = append(all, scored{entry: e, score: sim})
	}

	// Simple partial sort: keep top K
	results := make([]SearchResult, 0, topK)
	for _, s := range all {
		if len(results) < topK {
			results = append(results, SearchResult{
				MemoryName: s.entry.memoryName,
				ChunkIndex: s.entry.chunkIndex,
				ChunkText:  s.entry.chunkText,
				Score:      s.score,
			})
			// bubble up if needed (simple insertion sort for tiny K)
			for j := len(results) - 1; j > 0 && results[j].Score > results[j-1].Score; j-- {
				results[j], results[j-1] = results[j-1], results[j]
			}
		} else if s.score > results[topK-1].Score {
			// Replace lowest
			results[topK-1] = SearchResult{
				MemoryName: s.entry.memoryName,
				ChunkIndex: s.entry.chunkIndex,
				ChunkText:  s.entry.chunkText,
				Score:      s.score,
			}
			for j := topK - 1; j > 0 && results[j].Score > results[j-1].Score; j-- {
				results[j], results[j-1] = results[j-1], results[j]
			}
		}
	}

	// Filter out low-relevance results
	var filtered []SearchResult
	for _, r := range results {
		if r.Score > 0.3 {
			filtered = append(filtered, r)
		}
	}

	return filtered
}

// Export persists the vector store to JSON (for future SQLite persistence).
func (vs *VectorStore) Export() ([]byte, error) {
	type entry struct {
		MemoryName string    `json:"memory_name"`
		ChunkIndex int       `json:"chunk_index"`
		ChunkText  string    `json:"chunk_text"`
		Embedding  []float32 `json:"embedding"`
	}
	var entries []entry
	for _, e := range vs.entries {
		entries = append(entries, entry{
			MemoryName: e.memoryName,
			ChunkIndex: e.chunkIndex,
			ChunkText:  e.chunkText,
			Embedding:  e.embedding,
		})
	}
	return json.Marshal(entries)
}

// Import loads the vector store from JSON.
func (vs *VectorStore) Import(data []byte) error {
	type entry struct {
		MemoryName string    `json:"memory_name"`
		ChunkIndex int       `json:"chunk_index"`
		ChunkText  string    `json:"chunk_text"`
		Embedding  []float32 `json:"embedding"`
	}
	var entries []entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	vs.entries = nil
	for _, e := range entries {
		vs.entries = append(vs.entries, vectorEntry{
			memoryName: e.MemoryName,
			chunkIndex: e.ChunkIndex,
			chunkText:  e.ChunkText,
			embedding:  e.Embedding,
		})
	}
	return nil
}
