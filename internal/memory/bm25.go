package memory

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// BM25 is a keyword-based retrieval scorer. It calculates relevance between
// a query and a set of documents without requiring embedding vectors.
// This is the approach used by many agent frameworks (LangChain's BM25Retriever,
// mem0's keyword fallback, etc.) when embeddings are unavailable or too costly.
type BM25 struct {
	k1     float64 // term frequency saturation (default 1.2)
	b      float64 // length normalization (default 0.75)
	avgDL  float64 // average document length
	tokens []bm25Doc
}

type bm25Doc struct {
	id      int
	text    string
	tokens  []string
	tf      map[string]int
	length  int
	updated time.Time
}

// NewBM25 creates a BM25 scorer.
func NewBM25(k1, b float64) *BM25 {
	if k1 <= 0 {
		k1 = 1.2
	}
	if b <= 0 {
		b = 0.75
	}
	return &BM25{k1: k1, b: b}
}

// Clear removes all documents from the index.
func (b *BM25) Clear() {
	b.tokens = nil
	b.avgDL = 0
}

// Index adds or updates a document in the index.
func (b *BM25) Index(docID int, text string, updated time.Time) {
	// Remove existing document with same ID
	for i, d := range b.tokens {
		if d.id == docID {
			b.tokens = append(b.tokens[:i], b.tokens[i+1:]...)
			break
		}
	}

	tokens := tokenize(text)
	tf := make(map[string]int)
	for _, t := range tokens {
		tf[t]++
	}

	b.tokens = append(b.tokens, bm25Doc{
		id:      docID,
		text:    text,
		tokens:  tokens,
		tf:      tf,
		length:  len(tokens),
		updated: updated,
	})

	// Update average doc length
	var total int
	for _, d := range b.tokens {
		total += d.length
	}
	b.avgDL = float64(total) / float64(len(b.tokens))
}

// Search returns the top-K documents matching the query, sorted by BM25 score.
func (b *BM25) Search(query string, topK int) []ScoredDoc {
	queryTokens := tokenize(query)
	N := float64(len(b.tokens))

	if N == 0 {
		return nil
	}

	// Calculate IDF for each query token
	idf := make(map[string]float64)
	for _, qt := range queryTokens {
		var df float64
		for _, d := range b.tokens {
			if d.tf[qt] > 0 {
				df++
			}
		}
		idf[qt] = math.Log(1 + (N-df+0.5)/(df+0.5))
	}

	type scored struct {
		doc   bm25Doc
		score float64
	}

	var ranked []scored
	for _, doc := range b.tokens {
		score := b.scoreDoc(doc, queryTokens, idf)
		ranked = append(ranked, scored{doc, score})
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	var results []ScoredDoc
	for i, r := range ranked {
		if i >= topK {
			break
		}
		if r.score <= 0 {
			break
		}
		results = append(results, ScoredDoc{
			ID:      r.doc.id,
			Text:    r.doc.text,
			Score:   r.score,
			Updated: r.doc.updated,
		})
	}
	return results
}

// scoreDoc computes BM25 score for a single document.
func (b *BM25) scoreDoc(doc bm25Doc, queryTokens []string, idf map[string]float64) float64 {
	var score float64
	docLen := float64(doc.length)

	for _, qt := range queryTokens {
		tf := float64(doc.tf[qt])
		if tf == 0 {
			continue
		}

		// BM25 term score
		numerator := tf * (b.k1 + 1)
		denominator := tf + b.k1*(1-b.b+b.b*docLen/b.avgDL)
		termScore := numerator / denominator

		score += idf[qt] * termScore
	}

	return score
}

// ScoredDoc is a search result with relevance score and recency information.
type ScoredDoc struct {
	ID      int
	Text    string
	Score   float64
	Updated time.Time
}

// CombinedScore blends BM25 relevance with recency.
// Recent documents get a bonus; old documents get a slight penalty.
// decayHours controls how quickly recency bonus decays.
func (s ScoredDoc) CombinedScore(now time.Time, decayHours float64) float64 {
	hours := now.Sub(s.Updated).Hours()
	if hours < 0 {
		hours = 0
	}

	// Recency weight: 0.3, BM25 weight: 0.7
	recencyScore := math.Exp(-hours / decayHours)

	return 0.7*s.Score + 0.3*recencyScore
}

// tokenize splits text into lowercase tokens, removing punctuation.
func tokenize(text string) []string {
	lower := strings.ToLower(text)

	var tokens []string
	start := -1

	for i, r := range lower {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			if start < 0 {
				start = i
			}
		} else {
			if start >= 0 {
				tokens = append(tokens, lower[start:i])
				start = -1
			}
		}
	}
	if start >= 0 {
		tokens = append(tokens, lower[start:])
	}

	// Remove pure stopwords and short tokens
	var filtered []string
	for _, t := range tokens {
		if len(t) > 2 && !isStopword(t) {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

func isStopword(s string) bool {
	switch s {
	case "the", "and", "for", "are", "but", "not", "you", "all", "can",
		"had", "her", "was", "one", "our", "out", "has", "have", "been",
		"some", "than", "its", "his", "when", "will", "each", "about",
		"this", "that", "with", "from", "they", "what", "which",
		// Expanded coverage (docs/中等模型平替优化建议.md §2.2): the original
		// list missed several very common function words, which meant they
		// still consumed a "real" term slot in every BM25 query/document
		// without ever discriminating between documents. Deliberately does
		// NOT add code-relevant words like "function"/"class"/"struct" —
		// those are genuinely informative for a codebase-memory index.
		"were", "who", "why", "how", "let", "just", "into",
		"then", "them", "there", "these", "those", "over", "under",
		"such", "same", "only", "also", "very", "more", "most", "much",
		"here", "where", "while", "because", "before", "after", "again",
		"other", "would", "could", "should", "does", "did", "doing",
		"being", "having", "yourself", "yours", "your", "himself",
		"herself", "itself", "themselves", "ourselves":
		return true
	}
	return false
}
