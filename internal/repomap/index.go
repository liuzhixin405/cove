package repomap

import (
	"io/fs"
	"math"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/safepath"
)

// Index is the incremental repo map of one workspace: per-file definitions
// and references, kept across calls, plus the PageRank of every file in the
// reference graph. Refresh re-walks the tree but only re-parses files whose
// mtime or size changed, and recomputes the ranking only when something
// changed, so a query after an edit costs one walk and one parse.
//
// One Index per root is shared by every caller (IndexFor): the repo_map
// tool, the per-turn excerpt and the /context command all see the same
// state.
type Index struct {
	root string

	requested atomic.Int64 // refresh requests issued
	mu        sync.Mutex
	served    int64 // requests the last completed walk covers
	files     map[string]*fileEntry
	paths     []string // sorted keys of files
	rank      map[string]float64
	g         *refGraph // a -> b: a references what b defines
	und       *refGraph // g with every edge both ways, for PersonalRank
	lastUse   time.Time
}

type fileEntry struct {
	mtime time.Time
	size  int64
	fm    FileMap
	refs  map[string]int // identifier -> occurrences in this file
}

// RepoDiff captures repository changes found by one Refresh.
type RepoDiff struct {
	Added    []string
	Removed  []string
	Modified []string
}

// Changed reports whether the refresh found any change.
func (d *RepoDiff) Changed() bool {
	return d != nil && len(d.Added)+len(d.Removed)+len(d.Modified) > 0
}

// indexSkipDirs are never descended into, on top of every dot-directory.
var indexSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true, "build": true, "dist": true,
	"target": true, "bin": true, "obj": true, "__pycache__": true, "venv": true,
}

const (
	// indexMaxEntries bounds the walk (a session started in a home
	// directory must not scan the whole disk).
	indexMaxEntries = 100000
	// indexMaxFiles bounds how many source files are parsed.
	indexMaxFiles = 20000
	// indexMaxFileBytes: bigger files are generated or bundled code.
	indexMaxFileBytes = 1 << 20
	// indexRegistryMax bounds how many workspaces keep an Index in memory.
	indexRegistryMax = 8
)

var (
	registryMu sync.Mutex
	registry   = map[string]*Index{}
)

// IndexFor returns the shared Index of root, creating it on first use. At
// most indexRegistryMax roots are kept; the least recently used is dropped.
func IndexFor(root string) *Index {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	root = filepath.Clean(root)
	registryMu.Lock()
	defer registryMu.Unlock()
	ix := registry[root]
	if ix == nil {
		if len(registry) >= indexRegistryMax {
			var oldest string
			var at time.Time
			for k, v := range registry {
				if oldest == "" || v.lastUse.Before(at) {
					oldest, at = k, v.lastUse
				}
			}
			delete(registry, oldest)
		}
		ix = &Index{root: root, files: map[string]*fileEntry{}}
		registry[root] = ix
	}
	ix.lastUse = time.Now()
	return ix
}

// Root is the directory the index maps.
func (ix *Index) Root() string {
	if ix == nil {
		return ""
	}
	return ix.root
}

// Refresh brings the index up to date with the tree and returns what
// changed. Concurrent callers share one walk: a caller that finds a walk
// started after its own request has completed returns at once (the tool
// calls of one batch run in parallel).
func (ix *Index) Refresh() *RepoDiff {
	want := ix.requested.Add(1)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.served >= want {
		return &RepoDiff{}
	}
	covers := ix.requested.Load()

	current := ix.scan()
	diff := &RepoDiff{}
	var toParse []string
	for rel, st := range current {
		old, ok := ix.files[rel]
		switch {
		case !ok:
			diff.Added = append(diff.Added, rel)
			toParse = append(toParse, rel)
		case !old.mtime.Equal(st.mtime) || old.size != st.size:
			diff.Modified = append(diff.Modified, rel)
			toParse = append(toParse, rel)
		}
	}
	for rel := range ix.files {
		if _, ok := current[rel]; !ok {
			diff.Removed = append(diff.Removed, rel)
			delete(ix.files, rel)
		}
	}
	for rel, e := range ix.parseAll(toParse, current) {
		ix.files[rel] = e
	}
	sort.Strings(diff.Added)
	sort.Strings(diff.Modified)
	sort.Strings(diff.Removed)

	if diff.Changed() || ix.rank == nil {
		ix.rebuildRank()
	}
	ix.served = covers
	return diff
}

type fileStat struct {
	mtime time.Time
	size  int64
}

// scan walks the tree and returns the source files it maps.
func (ix *Index) scan() map[string]fileStat {
	out := map[string]fileStat{}
	seen := 0
	_ = filepath.WalkDir(ix.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != ix.root {
				return filepath.SkipDir
			}
			return nil
		}
		seen++
		if seen > indexMaxEntries || len(out) >= indexMaxFiles {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if p != ix.root && (strings.HasPrefix(name, ".") || indexSkipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !isScannedExt(ext) || strings.HasSuffix(name, ".min.js") || strings.HasSuffix(name, ".d.ts") {
			return nil
		}
		// A link to a file outside the project was parsed through os.Open,
		// which follows it, and its outline (names, signatures) went to the
		// model although read refuses that path. WalkDir itself does not
		// descend into linked directories.
		if d.Type()&fs.ModeSymlink != 0 && !safepath.Within(ix.root, p) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > indexMaxFileBytes {
			return nil
		}
		rel, err := filepath.Rel(ix.root, p)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = fileStat{mtime: info.ModTime(), size: info.Size()}
		return nil
	})
	return out
}

// parseAll parses rels on a few workers.
func (ix *Index) parseAll(rels []string, stats map[string]fileStat) map[string]*fileEntry {
	out := make(map[string]*fileEntry, len(rels))
	if len(rels) == 0 {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan string)
	workers := min(runtime.NumCPU(), len(rels))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range next {
				abs := filepath.Join(ix.root, filepath.FromSlash(rel))
				fm, refs := parseFile(abs, rel, strings.ToLower(filepath.Ext(rel)))
				st := stats[rel]
				mu.Lock()
				out[rel] = &fileEntry{mtime: st.mtime, size: st.size, fm: fm, refs: refs}
				mu.Unlock()
			}
		}()
	}
	for _, rel := range rels {
		next <- rel
	}
	close(next)
	wg.Wait()
	return out
}

// Files returns every mapped file that defines at least one symbol, in path
// order, with Score set to its global rank.
func (ix *Index) Files() []FileMap {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]FileMap, 0, len(ix.paths))
	for _, p := range ix.paths {
		e := ix.files[p]
		if len(e.fm.Symbols) == 0 {
			continue
		}
		fm := e.fm
		fm.Score = rankScore(ix.rank[p])
		out = append(out, fm)
	}
	return out
}

// rankScore turns a rank into FileMap.Score.
func rankScore(r float64) int { return int(math.Round(r * 1e6)) }

// PersonalRank ranks the files by PageRank personalised to seeds (path ->
// weight) over the reference graph taken both ways, so the files the seeds
// depend on and the files that depend on them (callers) both come out on
// top. Files unreachable from the seeds get 0.
func (ix *Index) PersonalRank(seeds map[string]float64) map[string]float64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.g == nil || len(seeds) == 0 {
		return nil
	}
	personal := make([]float64, len(ix.paths))
	total := 0.0
	for i, p := range ix.paths {
		if w := seeds[p]; w > 0 {
			personal[i] = w
			total += w
		}
	}
	if total == 0 {
		return nil
	}
	for i := range personal {
		personal[i] /= total
	}
	r := ix.und.pagerank(personal)
	out := make(map[string]float64, len(r))
	for i, v := range r {
		if v > 0 {
			out[ix.paths[i]] = v
		}
	}
	return out
}

// referencedBy returns the identifiers the files at paths reference.
func (ix *Index) referencedBy(paths []string) map[string]bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := map[string]bool{}
	for _, p := range paths {
		if e := ix.files[p]; e != nil {
			for id := range e.refs {
				out[id] = true
			}
		}
	}
	return out
}

// rebuildRank rebuilds the reference graph and the global (uniform) rank.
// Caller holds ix.mu.
func (ix *Index) rebuildRank() {
	ix.paths = ix.paths[:0]
	for p := range ix.files {
		ix.paths = append(ix.paths, p)
	}
	sort.Strings(ix.paths)
	ix.g = buildRefGraph(ix.paths, ix.files)
	ix.und = ix.g.undirected()
	uniform := make([]float64, len(ix.paths))
	for i := range uniform {
		uniform[i] = 1 / float64(len(uniform))
	}
	r := ix.g.pagerank(uniform)
	ix.rank = make(map[string]float64, len(r))
	for i, v := range r {
		ix.rank[ix.paths[i]] = v
	}
}

// refGraph is the file dependency graph: an edge a -> b means a names an
// identifier b defines. Everything is built in sorted order so the float
// arithmetic, and the ranking, is identical on every run.
type refGraph struct {
	out  [][]refEdge
	outW []float64
}

type refEdge struct {
	to int
	w  float64
}

// Edge weights, after Aider's repo map: an identifier defined in many files
// ("New", "String", "get") says little about which one a file depends on,
// and a private-looking one ("_helper") is a weaker link than a public API.
const (
	commonDefFiles = 5
	commonDefMul   = 0.1
	privateMul     = 0.1
)

func buildRefGraph(paths []string, files map[string]*fileEntry) *refGraph {
	defs := map[string][]int{}
	for i, p := range paths {
		seen := map[string]bool{}
		for _, s := range files[p].fm.Symbols {
			if len(s.Name) >= minRefIdentLen && !seen[s.Name] {
				seen[s.Name] = true
				defs[s.Name] = append(defs[s.Name], i)
			}
		}
	}
	g := &refGraph{out: make([][]refEdge, len(paths)), outW: make([]float64, len(paths))}
	for i, p := range paths {
		refs := files[p].refs
		ids := make([]string, 0, len(refs))
		for id := range refs {
			if len(defs[id]) > 0 {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		acc := map[int]float64{}
		for _, id := range ids {
			definers := defs[id]
			others := len(definers)
			for _, d := range definers {
				if d == i {
					others--
				}
			}
			if others == 0 {
				continue
			}
			mul := 1.0
			if len(definers) > commonDefFiles {
				mul *= commonDefMul
			}
			if strings.HasPrefix(id, "_") {
				mul *= privateMul
			}
			w := mul * math.Sqrt(float64(refs[id])) / float64(others)
			for _, d := range definers {
				if d != i {
					acc[d] += w
				}
			}
		}
		targets := make([]int, 0, len(acc))
		for d := range acc {
			targets = append(targets, d)
		}
		sort.Ints(targets)
		for _, d := range targets {
			g.out[i] = append(g.out[i], refEdge{to: d, w: acc[d]})
			g.outW[i] += acc[d]
		}
	}
	return g
}

// undirected returns g with every edge also added in reverse.
func (g *refGraph) undirected() *refGraph {
	n := len(g.out)
	acc := make([]map[int]float64, n)
	for i := range acc {
		acc[i] = map[int]float64{}
	}
	for i, es := range g.out {
		for _, e := range es {
			acc[i][e.to] += e.w
			acc[e.to][i] += e.w
		}
	}
	u := &refGraph{out: make([][]refEdge, n), outW: make([]float64, n)}
	for i, m := range acc {
		targets := make([]int, 0, len(m))
		for d := range m {
			targets = append(targets, d)
		}
		sort.Ints(targets)
		for _, d := range targets {
			u.out[i] = append(u.out[i], refEdge{to: d, w: m[d]})
			u.outW[i] += m[d]
		}
	}
	return u
}

const (
	pagerankDamping = 0.85
	pagerankIters   = 50
	pagerankEpsilon = 1e-10
)

// pagerank runs personalised PageRank; personal sums to 1. Rank of files
// with no outgoing edges is redistributed by personal.
func (g *refGraph) pagerank(personal []float64) []float64 {
	n := len(g.out)
	r := append([]float64(nil), personal...)
	next := make([]float64, n)
	for it := 0; it < pagerankIters; it++ {
		dangling := 0.0
		for i := range next {
			next[i] = 0
		}
		for i := 0; i < n; i++ {
			if g.outW[i] == 0 {
				dangling += r[i]
				continue
			}
			for _, e := range g.out[i] {
				next[e.to] += r[i] * e.w / g.outW[i]
			}
		}
		delta := 0.0
		for i := 0; i < n; i++ {
			v := (1-pagerankDamping)*personal[i] + pagerankDamping*(next[i]+dangling*personal[i])
			delta += math.Abs(v - r[i])
			next[i] = v
		}
		r, next = next, r
		if delta < pagerankEpsilon {
			break
		}
	}
	return r
}
