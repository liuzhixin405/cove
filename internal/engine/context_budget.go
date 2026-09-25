package engine

import (
	"sort"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove/internal/token"
)

// contextLayer is a priority tier for optional, potentially-large pieces
// of the system prompt. Lower-numbered layers are filled first; once the
// shared budget (see contextBudgeter) is exhausted, higher-numbered
// layers are truncated or dropped entirely. This replaces the previous
// approach where matched skill prompts, retrieved memories, the repo
// map, the file tree, and session notes each unconditionally appended
// their full content and simply hoped the combined total fit inside the
// model's context window — the "Context分层预算器" item in
// docs/中等模型平替优化建议.md.
type contextLayer int

const (
	// layerRelevant holds content that's already been scoped to what's
	// likely useful this turn — matched skill prompts, BM25/vector-
	// retrieved memories. Filled first because it was deliberately
	// selected, not just "everything available".
	layerRelevant contextLayer = 0
	// layerOnDemand holds broad situational overviews the model can
	// otherwise reconstruct with a single tool call (ls, read, grep) —
	// the project outline (the repo map itself is queried with the
	// repo_map tool). Losing this costs an extra tool round-trip, not
	// correctness.
	layerOnDemand contextLayer = 1
	// layerOverflow holds "nice to have" additions — session notes — that
	// are the first to be dropped under budget pressure.
	layerOverflow contextLayer = 2
)

type contextSection struct {
	layer   contextLayer
	content string
}

// contextBudgeter assembles named, priority-tagged sections into one
// string within a shared token budget. Sections are included layer by
// layer (lowest first); a section that only partially fits is truncated
// to a clean boundary via token.TruncateToTokens rather than either
// being silently corrupted or unconditionally appended regardless of
// size. Once the budget is exhausted, remaining sections are dropped.
//
// This is intentionally simple (no persistence, no cross-turn state) —
// it exists to stop five independent, unbounded string concatenations
// from competing for space with no ordering or ceiling, not to become a
// general-purpose context management subsystem.
type contextBudgeter struct {
	budget   int
	sections []contextSection
}

// newContextBudgeter creates a budgeter with the given total token
// budget. A non-positive budget falls back to a small conservative
// default rather than disabling budgeting altogether (an
// unbounded/zero budget here would silently reproduce the old
// "everything gets appended" behavior).
func newContextBudgeter(budgetTokens int) *contextBudgeter {
	if budgetTokens <= 0 {
		budgetTokens = 2000
	}
	return &contextBudgeter{budget: budgetTokens}
}

// add registers a section. Empty/whitespace-only content is ignored so
// callers don't need to guard every call site with their own emptiness
// check.
func (b *contextBudgeter) add(layer contextLayer, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	b.sections = append(b.sections, contextSection{layer: layer, content: content})
}

// Render returns the concatenation of sections that fit within the
// budget, ordered by layer (lowest first); insertion order is preserved
// within a layer via a stable sort.
func (b *contextBudgeter) Render() string {
	ordered := make([]contextSection, len(b.sections))
	copy(ordered, b.sections)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].layer < ordered[j].layer })

	var sb strings.Builder
	remaining := b.budget
	for _, s := range ordered {
		if remaining <= 0 {
			break
		}
		content := s.content
		est := token.Estimate(content)
		if est > remaining {
			content = token.TruncateToTokens(content, remaining)
			est = token.Estimate(content)
		}
		sb.WriteString(content)
		remaining -= est
	}
	return sb.String()
}

// Caps of the project context the engine adds on its own.
const (
	// memoryIndexMaxBytes caps the <memory_index> block of the system prompt
	// (the list of saved memories shown when they are too many to inline).
	// Entries past the cap are still found by the per-turn relevant-memory
	// note, which searches every memory.
	memoryIndexMaxBytes = 4096
	// repoMapExcerptMaxBytes caps the <repo_map_excerpt> turn note.
	repoMapExcerptMaxBytes = 12 * 1024
	// repoMapExcerptsPerSession is how many turns get an automatic repo map
	// excerpt: the first task-like one; later turns use the repo_map tool.
	repoMapExcerptsPerSession = 1
)

// capMemoryIndex cuts the <memory_index> block of a memory prompt to
// memoryIndexMaxBytes at entry boundaries, saying how many entries were left
// out. The rest of the prompt (instruction files, inlined memories) is kept.
func capMemoryIndex(prompt string) string {
	const open, closing = "<memory_index>\n", "</memory_index>\n"
	start := strings.Index(prompt, open)
	if start < 0 {
		return prompt
	}
	rel := strings.Index(prompt[start:], closing)
	if rel < 0 {
		return prompt
	}
	end := start + rel + len(closing)
	if end-start <= memoryIndexMaxBytes {
		return prompt
	}
	body := prompt[start+len(open) : start+rel]
	lines := strings.SplitAfter(body, "\n")
	var kept strings.Builder
	used := len(open) + len(closing) + 80 // room for the "more" line
	dropped := 0
	for i, l := range lines {
		if l == "" {
			continue
		}
		if dropped > 0 || (i > 0 && used+len(l) > memoryIndexMaxBytes) {
			dropped++
			continue
		}
		used += len(l)
		kept.WriteString(l)
	}
	if dropped > 0 {
		kept.WriteString("- … " + strconv.Itoa(dropped) + " more saved memories (list the memory directory to see them all)\n")
	}
	return prompt[:start] + open + kept.String() + closing + prompt[end:]
}
