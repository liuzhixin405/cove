package repomap

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// queryMaxSymbolsPerFile bounds the symbols listed for one file in a query
// result; matching symbols are listed first.
const queryMaxSymbolsPerFile = 30

// queryMaxTerms bounds the terms a query (or ExtractTerms) uses.
const queryMaxTerms = 12

// Query returns, in at most budget bytes, the repo map of the files under
// root that match terms: a term matches a file whose path contains it, or a
// symbol whose name contains it (case-insensitive). Files are ranked by how
// well they match, source above tests unless a term asks for tests, then by
// the reference ranking of BuildRanked. With no terms it lists the
// top-ranked files. Empty when nothing matches.
func Query(root string, terms []string, budget int) string {
	return QueryIn(root, "", terms, budget)
}

type queryHit struct {
	fm      FileMap
	score   int
	isTest  bool
	matched map[int]bool // indexes of matching symbols
}

// QueryIn is Query restricted to the files at or below pathPrefix (relative
// to root, or absolute inside it).
func QueryIn(root, pathPrefix string, terms []string, budget int) string {
	if budget <= 0 {
		return ""
	}
	prefix := normalizePrefix(root, pathPrefix)
	terms = normalizeTerms(terms)
	wantTests := false
	for _, t := range terms {
		if strings.Contains(t, "test") || strings.Contains(t, "spec") {
			wantTests = true
		}
	}

	var hits []queryHit
	for _, fm := range NewGenerator(root).BuildRanked(math.MaxInt) {
		if prefix != "" && fm.Path != prefix && !strings.HasPrefix(fm.Path, prefix+"/") {
			continue
		}
		h := queryHit{fm: fm, isTest: isTestPath(fm.Path), matched: map[int]bool{}}
		lpath := strings.ToLower(fm.Path)
		base := strings.TrimSuffix(filepath.Base(lpath), filepath.Ext(lpath))
		for _, t := range terms {
			if strings.Contains(lpath, t) {
				h.score += 4
				if base == t {
					h.score += 2
				}
			}
			symScore := 0
			for i, s := range fm.Symbols {
				ln := strings.ToLower(s.Name)
				switch {
				case ln == t:
					symScore += 6
					h.matched[i] = true
				case ln != "" && strings.Contains(ln, t):
					symScore += 2
					h.matched[i] = true
				}
			}
			h.score += min(symScore, 12)
		}
		if len(terms) > 0 && h.score == 0 {
			continue
		}
		hits = append(hits, h)
	}
	if len(hits) == 0 {
		return ""
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if !wantTests && a.isTest != b.isTest {
			return !a.isTest
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.fm.Score != b.fm.Score {
			return a.fm.Score > b.fm.Score
		}
		return a.fm.Path < b.fm.Path
	})

	// Room is kept for the "… N more matching files" marker, so a cut
	// result always says it was cut.
	const markerRoom = 96
	limit := budget
	if len(hits) > 1 && budget > 2*markerRoom {
		limit = budget - markerRoom
	}
	var sb strings.Builder
	shown := 0
	for _, h := range hits {
		block := formatHit(h)
		if sb.Len()+len(block) > limit {
			if shown == 0 {
				// The best file alone is over budget: keep what fits of it.
				sb.WriteString(cutAtLine(block, limit))
				shown++
			}
			continue
		}
		sb.WriteString(block)
		shown++
	}
	if rest := len(hits) - shown; rest > 0 {
		more := fmt.Sprintf("… %d more matching files (narrow the query or pass path)\n", rest)
		if sb.Len()+len(more) <= budget {
			sb.WriteString(more)
		}
	}
	return sb.String()
}

// formatHit renders one file like FormatFileMaps, matching symbols first and
// at most queryMaxSymbolsPerFile of them.
func formatHit(h queryHit) string {
	var sb strings.Builder
	sb.WriteString(h.fm.Path)
	if h.fm.Package != "" {
		sb.WriteString(" (package " + h.fm.Package + ")")
	}
	sb.WriteString(":\n")
	order := make([]int, 0, len(h.fm.Symbols))
	for i := range h.fm.Symbols {
		if h.matched[i] {
			order = append(order, i)
		}
	}
	for i := range h.fm.Symbols {
		if !h.matched[i] {
			order = append(order, i)
		}
	}
	for n, i := range order {
		if n == queryMaxSymbolsPerFile {
			fmt.Fprintf(&sb, "  … %d more symbols\n", len(order)-n)
			break
		}
		s := h.fm.Symbols[i]
		prefix := "  - "
		if s.Type == "method" {
			prefix = "    "
		}
		fmt.Fprintf(&sb, "%s%s  :%d\n", prefix, s.Signature, s.Line)
	}
	sb.WriteString("\n")
	return sb.String()
}

func normalizePrefix(root, p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return ""
	}
	if filepath.IsAbs(p) {
		if rel, err := filepath.Rel(root, p); err == nil {
			p = rel
		}
	}
	p = filepath.ToSlash(filepath.Clean(p))
	p = strings.TrimPrefix(p, "./")
	if p == "." {
		return ""
	}
	return strings.TrimSuffix(p, "/")
}

func normalizeTerms(terms []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(t, "\\", "/")))
		t = strings.TrimPrefix(t, "./")
		t = strings.Trim(t, "`'\"(),;:")
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == queryMaxTerms {
			break
		}
	}
	return out
}

func isTestPath(p string) bool {
	base := filepath.Base(p)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.HasPrefix(p, "test/") || strings.HasPrefix(p, "tests/") ||
		strings.Contains(p, "/test/") || strings.Contains(p, "/tests/")
}

var (
	termBacktick = regexp.MustCompile("`([^`\n]+)`")
	termPath     = regexp.MustCompile(`[A-Za-z0-9_.\-]+(?:[/\\][A-Za-z0-9_.\-]+)+|[A-Za-z0-9_\-]+\.(?:go|py|ts|tsx|js|jsx|mjs|rs|java|kt|cs|rb|php|swift|c|h|cpp|hpp|md|json|ya?ml|toml|sql|sh|ps1)\b`)
	termIdent    = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)
)

// termStopwords are English words too common to narrow a repo query.
var termStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true, "that": true, "from": true,
	"into": true, "are": true, "was": true, "you": true, "can": true, "not": true, "but": true,
	"how": true, "why": true, "what": true, "when": true, "where": true, "which": true, "please": true,
	"fix": true, "implement": true, "refactor": true, "add": true, "make": true, "use": true,
	"code": true, "file": true, "files": true, "function": true, "method": true, "should": true,
	"does": true, "have": true, "has": true, "all": true, "any": true, "some": true, "there": true,
	"then": true, "than": true, "also": true, "just": true, "like": true, "will": true, "would": true,
	"could": true, "need": true, "want": true, "let": true, "see": true, "look": true, "check": true,
}

// ExtractTerms picks the query terms of a request: file paths, backticked
// code, and identifiers (ASCII words of 3+ characters that are not common
// English words), in order of appearance, deduplicated, at most
// queryMaxTerms. Chinese text yields no terms by itself.
func ExtractTerms(text string) []string {
	var terms []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.Trim(t, ".-/\\")
		lt := strings.ToLower(t)
		if len(t) < 3 || seen[lt] || termStopwords[lt] || len(terms) >= queryMaxTerms {
			return
		}
		seen[lt] = true
		terms = append(terms, t)
	}
	rest := text
	for _, m := range termBacktick.FindAllStringSubmatch(text, -1) {
		inner := strings.TrimSpace(m[1])
		if termPath.MatchString(inner) && termPath.FindString(inner) == inner {
			add(strings.ReplaceAll(inner, "\\", "/"))
		} else {
			for _, id := range termIdent.FindAllString(inner, -1) {
				add(id)
			}
		}
		rest = strings.Replace(rest, m[0], " ", 1)
	}
	for _, p := range termPath.FindAllString(rest, -1) {
		add(strings.ReplaceAll(p, "\\", "/"))
	}
	rest = termPath.ReplaceAllString(rest, " ")
	for _, id := range termIdent.FindAllString(rest, -1) {
		add(id)
	}
	return terms
}
