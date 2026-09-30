package dream

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// Stale-reference check: before each consolidation, every memory of the
// project memory directory is checked for the paths and code symbols it
// names. Memories are written automatically (turn-end extraction, dream,
// skill review), and one that names a file since moved or a function since
// renamed keeps steering later sessions wrong. A memory with references that
// no longer resolve gets a marker line under its frontmatter, which every
// later read shows the model; the marker goes again once they resolve. The
// consolidation prompt lists them so the dream run fixes or deletes them.

// staleMarkerPrefix starts the marker line.
const staleMarkerPrefix = "> [stale-check "

// staleReport is one memory with references that no longer resolve.
type staleReport struct {
	File    string
	Missing []string
}

const (
	staleMaxWalkEntries = 60000
	staleMaxSearchFiles = 20000
	staleMaxSearchBytes = 64 << 20
	staleMaxFileBytes   = 1 << 20
)

var (
	staleBacktick = regexp.MustCompile("`([^`\n]{2,200})`")
	stalePath     = regexp.MustCompile(`(?:[A-Za-z]:)?[A-Za-z0-9_.\-]+(?:[/\\][A-Za-z0-9_.\-]+)+`)
	staleFileName = regexp.MustCompile(`^[A-Za-z0-9_.\-]+\.[A-Za-z][A-Za-z0-9]{0,5}$`)
	staleSymbol   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?(?:\(\))?$`)
	staleLineSufx = regexp.MustCompile(`:\d+(?::\d+)?$`)
)

// staleSkipDirs are not searched for symbols (on top of dot-directories).
var staleSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "bin": true, "obj": true, "dist": true, "build": true,
	"target": true, "__pycache__": true, "venv": true,
}

// staleSearchExt are the files a symbol may live in. Prose (.md, .txt) is
// left out: a changelog naming a removed function must not keep it alive.
var staleSearchExt = map[string]bool{
	".go": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".cs": true, ".java": true, ".kt": true, ".rs": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true,
	".rb": true, ".php": true, ".swift": true, ".vue": true, ".json": true, ".yaml": true, ".yml": true,
	".toml": true, ".xml": true, ".sql": true, ".sh": true, ".ps1": true, ".csproj": true, ".proto": true,
}

// markStaleMemories checks the memories in memDir against projectRoot,
// updates their markers and returns the memories with references that no
// longer resolve.
func markStaleMemories(projectRoot, memDir string, now time.Time) []staleReport {
	if projectRoot == "" || memDir == "" {
		return nil
	}
	if st, err := os.Stat(projectRoot); err != nil || !st.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return nil
	}
	type memFile struct {
		path, body string
		paths      []string
		names      []string // bare file names
		symbols    []string
	}
	var mems []*memFile
	wantNames, wantSymbols := map[string]bool{}, map[string]bool{}
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.EqualFold(filepath.Ext(name), ".md") || strings.EqualFold(name, entrypointName) || strings.EqualFold(name, "MEMORY.md") {
			continue
		}
		p := filepath.Join(memDir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		m := &memFile{path: p, body: string(data)}
		m.paths, m.names, m.symbols = memoryRefs(stripStaleMarker(m.body))
		for _, n := range m.names {
			wantNames[n] = true
		}
		for _, s := range m.symbols {
			wantSymbols[s] = true
		}
		mems = append(mems, m)
	}
	if len(mems) == 0 {
		return nil
	}
	foundNames, foundSymbols := searchProjectFn(projectRoot, wantNames, wantSymbols)

	var reports []staleReport
	for _, m := range mems {
		var missing []string
		for _, p := range m.paths {
			if !projectPathExists(projectRoot, p) {
				missing = append(missing, p)
			}
		}
		for _, n := range m.names {
			if !foundNames[n] {
				missing = append(missing, n)
			}
		}
		for _, s := range m.symbols {
			if !foundSymbols[s] {
				missing = append(missing, s)
			}
		}
		sort.Strings(missing)
		writeStaleMarker(m.path, m.body, missing, now)
		if len(missing) > 0 {
			reports = append(reports, staleReport{File: m.path, Missing: missing})
		}
	}
	return reports
}

// searchProjectFn is searchProject; a variable so tests can act while the
// walk runs.
var searchProjectFn = searchProject

// writeStaleMarker sets the marker on the memory at path. The body read
// before the project walk is not what gets written back: the walk can take
// a while, and a turn-end extraction appending to the memory meanwhile used
// to be erased by writing the marker onto the old copy. The file is re-read
// under the memory write locks (this process's and the memory directory's
// lock file, which an extraction in another process takes) and the marker
// applied to what it holds now.
func writeStaleMarker(path, readBody string, missing []string, now time.Time) {
	unlock, err := lockMemoryDir(filepath.Dir(path))
	if err != nil {
		// Skipping the marker costs nothing lasting: the next consolidation
		// checks again. Writing without the lock could erase an append.
		log.Warnf("[dream] stale marker for %s skipped: %v", path, err)
		return
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return // removed meanwhile: nothing to mark
	}
	current := string(data)
	if current != readBody {
		log.Debugf("[dream] %s changed during the stale check; marking its current content", path)
	}
	updated := setStaleMarker(current, missing, now)
	if updated == current {
		return
	}
	if err := fsatomic.WriteFile(path, []byte(updated), 0o644); err != nil {
		log.Warnf("[dream] stale marker for %s: %v", path, err)
	}
}

// memoryRefs extracts from a memory the project paths it names (with a
// directory part), the bare file names in backticks, and the code symbols in
// backticks (camelCase, snake_case, Type.Method or name()).
func memoryRefs(body string) (paths, names, symbols []string) {
	seen := map[string]bool{}
	addPath := func(p string) {
		p = strings.TrimRight(staleLineSufx.ReplaceAllString(strings.Trim(p, ".,;:()[]'\""), ""), ".,;:")
		if p == "" || seen["p:"+p] || strings.ContainsAny(p, "*?<>{}$~") {
			return
		}
		seen["p:"+p] = true
		paths = append(paths, p)
	}
	// Paths anywhere in the text (URLs excluded).
	for _, loc := range stalePath.FindAllStringIndex(body, -1) {
		if loc[0] >= 3 && body[loc[0]-3:loc[0]] == "://" {
			continue
		}
		if loc[0] > 0 && strings.ContainsRune("~$%/.", rune(body[loc[0]-1])) {
			continue // ~/.cove/..., $HOME/..., %APPDATA%\...
		}
		tok := body[loc[0]:loc[1]]
		if strings.Contains(tok, "://") || strings.HasPrefix(tok, "www.") || !strings.Contains(filepath.Base(filepath.FromSlash(strings.ReplaceAll(tok, "\\", "/"))), ".") {
			// Only paths that end in a file name: "a/b" is too often not a path.
			continue
		}
		addPath(tok)
	}
	for _, m := range staleBacktick.FindAllStringSubmatch(body, -1) {
		tok := strings.TrimSpace(m[1])
		tok = staleLineSufx.ReplaceAllString(tok, "")
		if staleFileName.MatchString(tok) && !looksLikeSymbol(tok) {
			if !seen["n:"+tok] {
				seen["n:"+tok] = true
				names = append(names, tok)
			}
			continue
		}
		if looksLikeSymbol(tok) {
			sym := strings.TrimSuffix(tok, "()")
			if i := strings.LastIndexByte(sym, '.'); i >= 0 {
				sym = sym[i+1:]
			}
			if len(sym) >= 4 && !seen["s:"+sym] {
				seen["s:"+sym] = true
				symbols = append(symbols, sym)
			}
		}
	}
	return paths, names, symbols
}

// looksLikeSymbol reports whether a backticked token reads as a code
// identifier rather than a word, a command or a file name: camelCase or
// PascalCase with an inner capital, snake_case, Type.Method or a call.
func looksLikeSymbol(tok string) bool {
	if !staleSymbol.MatchString(tok) {
		return false
	}
	if strings.HasSuffix(tok, "()") {
		return true
	}
	if i := strings.IndexByte(tok, '.'); i >= 0 {
		// Type.Method; "file.go" is a file name (lower-case extension).
		ext := tok[i+1:]
		return len(ext) > 0 && ext[0] >= 'A' && ext[0] <= 'Z'
	}
	if strings.Contains(strings.Trim(tok, "_"), "_") {
		return true
	}
	for i := 1; i < len(tok); i++ {
		if tok[i] >= 'A' && tok[i] <= 'Z' {
			return true
		}
	}
	return false
}

// projectPathExists reports whether p resolves inside root. A relative path
// whose first component is not in root at all is taken to be about some
// other place (~/.cove/sessions, another repository) and counts as existing:
// only a path into the project can go stale.
func projectPathExists(root, p string) bool {
	p = filepath.FromSlash(strings.ReplaceAll(p, "\\", "/"))
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true // outside the project
		}
		_, err = os.Stat(p)
		return err == nil
	}
	first := strings.SplitN(filepath.ToSlash(p), "/", 2)[0]
	if _, err := os.Stat(filepath.Join(root, first)); err != nil {
		return true
	}
	_, err := os.Stat(filepath.Join(root, p))
	return err == nil
}

// searchProject walks root once and reports which of names exist as a file
// name and which of symbols occur in a source or config file.
func searchProject(root string, names, symbols map[string]bool) (foundNames, foundSymbols map[string]bool) {
	foundNames, foundSymbols = map[string]bool{}, map[string]bool{}
	if len(names) == 0 && len(symbols) == 0 {
		return
	}
	pending := make([]string, 0, len(symbols))
	for s := range symbols {
		pending = append(pending, s)
	}
	seen, searched, read := 0, 0, 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		seen++
		if seen > staleMaxWalkEntries {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || staleSkipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if names[name] {
			foundNames[name] = true
		}
		if len(pending) == 0 || searched >= staleMaxSearchFiles || read >= staleMaxSearchBytes ||
			!staleSearchExt[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > staleMaxFileBytes {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		searched++
		read += len(data)
		text := string(data)
		rest := pending[:0]
		for _, s := range pending {
			if strings.Contains(text, s) {
				foundSymbols[s] = true
			} else {
				rest = append(rest, s)
			}
		}
		pending = rest
		return nil
	})
	// A search that stopped early proves nothing about what it did not
	// reach: count the rest as found rather than mark memories wrongly.
	if seen > staleMaxWalkEntries || searched >= staleMaxSearchFiles || read >= staleMaxSearchBytes {
		for _, s := range pending {
			foundSymbols[s] = true
		}
		for n := range names {
			foundNames[n] = true
		}
	}
	return
}

// stripStaleMarker removes the marker line from a memory.
func stripStaleMarker(body string) string {
	lines := strings.Split(body, "\n")
	out := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(l, staleMarkerPrefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// setStaleMarker puts the marker for missing under the memory's frontmatter
// (or at the top), replacing an older one; with nothing missing it removes
// the marker. The date is kept when the missing list did not change.
func setStaleMarker(body string, missing []string, now time.Time) string {
	var old string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, staleMarkerPrefix) {
			old = l
			break
		}
	}
	clean := stripStaleMarker(body)
	if len(missing) == 0 {
		return clean
	}
	quoted := make([]string, len(missing))
	for i, m := range missing {
		quoted[i] = "`" + m + "`"
	}
	tail := "] Not found in the project any more: " + strings.Join(quoted, ", ") + ". Check before relying on this memory; update or delete it."
	marker := staleMarkerPrefix + now.Format("2006-01-02") + tail
	if old != "" && strings.HasSuffix(old, tail) {
		marker = old
	}
	if strings.HasPrefix(clean, "---\n") || strings.HasPrefix(clean, "---\r\n") {
		if end := strings.Index(clean[3:], "\n---"); end >= 0 {
			cut := 3 + end + len("\n---")
			if nl := strings.IndexByte(clean[cut:], '\n'); nl >= 0 {
				cut += nl + 1
				return clean[:cut] + marker + "\n" + clean[cut:]
			}
			return clean + "\n" + marker + "\n"
		}
	}
	return marker + "\n" + clean
}

// staleSection is the consolidation prompt's list of stale memories.
func staleSection(reports []staleReport) string {
	if len(reports) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n**Memories whose references no longer resolve** (a check of the project just now; each carries a \"" +
		strings.TrimSpace(staleMarkerPrefix) + "\" line). For each, look at the project: update the memory to the current name or path, or delete it (and its index line) if it no longer holds, and remove the marker line. " +
		"Only backticked names are checked as code symbols: if a flagged name was never part of the project (a product, an external API), write it without backticks.\n")
	for _, r := range reports {
		sb.WriteString("- " + r.File + ": " + strings.Join(r.Missing, ", ") + "\n")
	}
	return sb.String()
}
