package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MaxIndexLines is the maximum number of lines for any single memory file.
	MaxIndexLines = 200
	// MaxEntryBytes is the maximum size in bytes per memory entry.
	MaxEntryBytes = 25 * 1024 // 25KB
	// MaxTotalBytes is the maximum total size across all memory files.
	MaxTotalBytes = 100 * 1024 // 100KB
)

type Store struct {
	dirs []string

	// BM25 keyword search (no embedding required)
	bm25 *BM25

	// Cache to avoid repeated disk reads on every system prompt build
	mu          sync.Mutex
	cachedAll   []Entry
	cacheTime   time.Time
	cacheTTL    time.Duration
	promptCache string
	promptDirty bool

	// Optional semantic search (opt-in; nil by default = pure BM25, the
	// original and still fully-supported behavior). See
	// EnableRemoteEmbeddings and docs/中等模型平替优化建议.md §2.2 — this
	// deliberately does NOT require any locally-installed model.
	embedProvider     EmbeddingProvider
	embedCache        map[string][]float32 // content-hash -> embedding vector
	embedBackoffUntil time.Time
}

// embedBackoffDuration bounds how often a failing/unreachable embeddings
// endpoint is retried: one slow failure costs one call, not every
// subsequent prompt build until the user notices and fixes their config.
const embedBackoffDuration = 5 * time.Minute

// EnableRemoteEmbeddings opts the store into blending BM25 keyword search
// with real semantic similarity from the given EmbeddingProvider (typically
// a RemoteAPIEmbeddingProvider — see embed.go). Call this only when the
// user has explicitly configured an embeddings endpoint; leaving it unset
// keeps the original pure-BM25 behavior with zero extra network calls or
// cost. Passing nil disables it again.
func (s *Store) EnableRemoteEmbeddings(provider EmbeddingProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embedProvider = provider
	if provider != nil && s.embedCache == nil {
		s.embedCache = make(map[string][]float32)
	}
}

func NewStore() *Store {
	home, _ := os.UserHomeDir()
	return &Store{
		dirs: []string{
			filepath.Join(home, ".cove", "memory"),
		},
		cacheTTL:    30 * time.Second,
		promptDirty: true,
	}
}

func (s *Store) AddDir(dir string) {
	s.dirs = append(s.dirs, dir)
}

// contentHash keys the embedding cache by content, so editing a memory
// file naturally invalidates its old cached vector without any explicit
// cache-invalidation bookkeeping.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:16])
}

// vectorScoreWeight is how much a perfect (cosine similarity 1.0) semantic
// match can add on top of the existing BM25+recency score. It is additive
// rather than a full re-normalized blend deliberately: BM25 scores are
// unbounded and query-dependent, so mixing them with a 0-1 cosine score via
// fixed weights would require normalizing BM25 first (another source of
// bugs); adding a bounded bonus preserves BM25's existing ranking behavior
// exactly when semantic search is unavailable (bonus is simply 0) and only
// nudges results when it is.
const vectorScoreWeight = 0.5

// vectorScores returns cosine similarity between query and each entry in
// entries (by slice index), using the configured EmbeddingProvider. It
// returns nil — not an error — whenever semantic search isn't usable for
// this call (disabled, backing off after a recent failure, or the API call
// itself failed just now), so callers can unconditionally add
// vectorScoreWeight*score without a separate enabled/disabled branch.
func (s *Store) vectorScores(ctx context.Context, query string, entries []Entry) map[int]float64 {
	s.mu.Lock()
	provider := s.embedProvider
	backoff := s.embedBackoffUntil
	s.mu.Unlock()

	if provider == nil || len(entries) == 0 {
		return nil
	}
	if time.Now().Before(backoff) {
		return nil
	}

	type pendingEntry struct {
		idx  int
		hash string
	}
	hashes := make([]string, len(entries))
	var toEmbed []pendingEntry

	s.mu.Lock()
	for i, e := range entries {
		h := contentHash(e.Content)
		hashes[i] = h
		if _, ok := s.embedCache[h]; !ok {
			toEmbed = append(toEmbed, pendingEntry{idx: i, hash: h})
		}
	}
	s.mu.Unlock()

	// Batch the query plus any not-yet-cached entries into one API call.
	inputs := make([]string, 0, 1+len(toEmbed))
	inputs = append(inputs, query)
	for _, p := range toEmbed {
		inputs = append(inputs, entries[p.idx].Content)
	}

	vecs, err := provider.Embed(ctx, inputs)
	if err != nil || len(vecs) == 0 || vecs[0] == nil {
		s.mu.Lock()
		s.embedBackoffUntil = time.Now().Add(embedBackoffDuration)
		s.mu.Unlock()
		return nil
	}
	queryVec := vecs[0]

	s.mu.Lock()
	for i, p := range toEmbed {
		if 1+i < len(vecs) && vecs[1+i] != nil {
			s.embedCache[p.hash] = vecs[1+i]
		}
	}
	// Simplest possible bound on unbounded growth (renamed/deleted memory
	// files leave stale entries behind): reset rather than partial-evict.
	if len(s.embedCache) > 2000 {
		s.embedCache = make(map[string][]float32)
	}
	scores := make(map[int]float64, len(entries))
	for i, h := range hashes {
		if v, ok := s.embedCache[h]; ok {
			scores[i] = cosineSimilarity(queryVec, v)
		}
	}
	s.mu.Unlock()

	return scores
}

func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Return cached if fresh
	if s.cachedAll != nil && time.Since(s.cacheTime) < s.cacheTTL {
		return s.cachedAll
	}

	var entries []Entry
	seen := map[string]bool{}
	for _, dir := range s.dirs {
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			path := filepath.Join(dir, f.Name())
			if seen[path] {
				continue
			}
			seen[path] = true
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			entries = append(entries, Entry{
				Name:    f.Name(),
				Path:    path,
				Content: string(data),
			})
		}
	}

	cwd, _ := os.Getwd()
	if cwd != "" {
		s.loadCLAUDEMD(cwd, &entries, &seen)
		s.loadCLAUDEMD(filepath.Join(cwd, ".claude"), &entries, &seen)
	}

	s.cachedAll = entries
	s.cacheTime = time.Now()
	s.promptDirty = true
	return entries
}

func (s *Store) loadCLAUDEMD(dir string, entries *[]Entry, seen *map[string]bool) {
	path := filepath.Join(dir, "CLAUDE.md")
	if (*seen)[path] {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	(*seen)[path] = true
	*entries = append(*entries, Entry{
		Name:    "CLAUDE.md",
		Path:    path,
		Content: string(data),
		Project: true,
	})
}

func (s *Store) BuildPrompt() string {
	entries := s.All()
	if len(entries) == 0 {
		return ""
	}

	s.mu.Lock()
	if !s.promptDirty && s.promptCache != "" {
		cached := s.promptCache
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("\n\n<user_memories>\n")
	for _, e := range entries {
		sb.WriteString("<memory>\n")
		sb.WriteString("<name>" + e.Name + "</name>\n")
		sb.WriteString("<content>\n")
		sb.WriteString(e.Content)
		sb.WriteString("\n</content>\n")
		sb.WriteString("</memory>\n")
	}
	sb.WriteString("</user_memories>\n")

	result := sb.String()
	s.mu.Lock()
	s.promptCache = result
	s.promptDirty = false
	s.mu.Unlock()
	return result
}

// EntryMatch is one ranked memory entry returned by Search.
type EntryMatch struct {
	Entry Entry
	Score float64
}

// Search runs BM25 keyword retrieval over all memory entries and returns the
// top matches ranked by relevance (blended with recency). No embeddings are
// used. Results are deduplicated by entry name.
func (s *Store) Search(query string, topK int) []EntryMatch {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if topK <= 0 {
		topK = 5
	}
	entries := s.All()
	if len(entries) == 0 {
		return nil
	}

	s.mu.Lock()
	if s.bm25 == nil {
		s.bm25 = NewBM25(1.2, 0.75)
	}
	bm25 := s.bm25
	s.mu.Unlock()

	now := time.Now()
	bm25.Clear()
	for i, e := range entries {
		bm25.Index(i, e.Content, now)
	}

	scored := bm25.Search(query, topK*2)
	// Optional semantic re-ranking bonus (nil map when disabled/unavailable
	// — see vectorScores' doc comment), computed once over the BM25
	// candidate set rather than the full corpus, since embedding every
	// memory entry on every search would be wasteful.
	vecScores := s.vectorScores(context.Background(), query, entries)

	seen := make(map[string]bool)
	var results []EntryMatch
	for _, r := range scored {
		if r.ID < 0 || r.ID >= len(entries) {
			continue
		}
		e := entries[r.ID]
		if seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		score := r.CombinedScore(now, 48)
		if vecScores != nil {
			if v, ok := vecScores[r.ID]; ok {
				score += vectorScoreWeight * v
			}
		}
		results = append(results, EntryMatch{Entry: e, Score: score})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if len(results) > topK {
		results = results[:topK]
	}
	return results
}

// Stats summarizes the memory store contents for observability.
type Stats struct {
	FileCount     int
	ProjectCount  int
	TotalBytes    int
	TotalLines    int
	MaxEntryBytes int
	MaxTotalBytes int
}

// Stats returns aggregate statistics over all memory entries.
func (s *Store) Stats() Stats {
	entries := s.All()
	st := Stats{
		MaxEntryBytes: MaxEntryBytes,
		MaxTotalBytes: MaxTotalBytes,
	}
	for _, e := range entries {
		st.FileCount++
		if e.Project {
			st.ProjectCount++
		}
		st.TotalBytes += len(e.Content)
		st.TotalLines += strings.Count(e.Content, "\n") + 1
	}
	return st
}

type Entry struct {
	Name    string
	Path    string
	Content string
	Project bool
}

func (s *Store) Save(name, content string) error {
	// Validate entry size
	if len(content) > MaxEntryBytes {
		return fmt.Errorf("memory entry %q exceeds max size (%d > %d bytes)", name, len(content), MaxEntryBytes)
	}
	// Validate line count
	lines := strings.Count(content, "\n") + 1
	if lines > MaxIndexLines {
		return fmt.Errorf("memory entry %q exceeds max lines (%d > %d)", name, lines, MaxIndexLines)
	}

	var dir string
	for _, d := range s.dirs {
		if _, err := os.Stat(d); err == nil {
			dir = d
			break
		}
	}
	if dir == "" {
		dir = s.dirs[0]
		os.MkdirAll(dir, 0700)
	}

	// Check total size would not exceed limit
	totalSize := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err == nil {
			if e.Name() == name {
				continue // replacing this file, don't count old size
			}
			totalSize += int(info.Size())
		}
	}
	if totalSize+len(content) > MaxTotalBytes {
		return fmt.Errorf("total memory size would exceed limit (%d + %d > %d bytes)", totalSize, len(content), MaxTotalBytes)
	}

	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	if err == nil {
		s.invalidateCache()
	}
	return err
}

// TruncateEntry truncates content to fit within limits.
func TruncateEntry(content string) string {
	// Truncate by lines
	lines := strings.Split(content, "\n")
	if len(lines) > MaxIndexLines {
		lines = lines[:MaxIndexLines]
		content = strings.Join(lines, "\n") + "\n... [truncated to 200 lines]"
	}
	// Truncate by bytes
	if len(content) > MaxEntryBytes {
		content = content[:MaxEntryBytes-50] + "\n... [truncated to 25KB]"
	}
	return content
}

func (s *Store) Delete(name string) error {
	for _, d := range s.dirs {
		path := filepath.Join(d, name)
		if _, err := os.Stat(path); err == nil {
			err := os.Remove(path)
			if err == nil {
				s.invalidateCache()
			}
			return err
		}
	}
	return nil
}

func (s *Store) invalidateCache() {
	s.mu.Lock()
	s.cachedAll = nil
	s.cacheTime = time.Time{}
	s.promptDirty = true
	s.promptCache = ""
	s.mu.Unlock()
}
