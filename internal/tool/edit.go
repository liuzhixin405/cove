package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type EditTool struct{ baseTool }

func NewEditTool() Tool {
	return &EditTool{baseTool{def: Def{
		Name: "edit", Aliases: []string{"Edit"},
		Description: "Make exact string replacements in files. Fails if oldString is not found or matches multiple times (use replaceAll for multiple).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filePath":{"type":"string","description":"Absolute path to the file to edit"},
				"oldString":{"type":"string","description":"The exact text to replace"},
				"newString":{"type":"string","description":"The text to replace with (must differ from oldString)"},
				"replaceAll":{"type":"boolean","description":"Replace all occurrences"}
			},
			"required":["filePath","oldString","newString"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Edit",
	}}}
}

func (t *EditTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	path, _ := input["filePath"].(string)
	oldS, _ := input["oldString"].(string)
	newS, _ := input["newString"].(string)
	all, _ := input["replaceAll"].(bool)

	if path == "" {
		return Result{Data: "Error: filePath required", IsError: true}, nil
	}
	if oldS == newS {
		return Result{Data: "Error: oldString and newString must differ", IsError: true}, nil
	}
	if oldS == "" {
		return Result{Data: "Error: oldString is empty", IsError: true}, nil
	}

	var err error
	path, err = resolvePathInCwd(path, tctx, true)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Data: "Error: file not found: " + path, IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	content := string(data)
	count := strings.Count(content, oldS)

	var result string
	replacements := 0

	switch {
	case count == 1, count > 1 && all:
		// Exact match path — unchanged behavior, zero risk to the existing
		// (already working) case.
		if all {
			result = strings.ReplaceAll(content, oldS, newS)
			replacements = count
		} else {
			result = strings.Replace(content, oldS, newS, 1)
			replacements = 1
		}
	case count > 1:
		return Result{Data: fmt.Sprintf("Error: oldString matches %d times. Use replaceAll=true or provide more context.", count), IsError: true}, nil
	default:
		// count == 0: mid-tier models frequently produce an oldString that is
		// semantically right but byte-for-byte wrong (extra/missing spaces,
		// tab vs. space indentation, CRLF vs LF, a trailing blank line). Try a
		// whitespace-normalized, line-aware match before giving up outright.
		fuzzyResult, applied, hint := fuzzyApplyEdit(content, oldS, newS)
		if applied {
			result = fuzzyResult
			replacements = 1
		} else {
			msg := "Error: oldString not found in file."
			if hint != "" {
				msg += " " + hint
			}
			return Result{Data: msg, IsError: true}, nil
		}
	}

	perm := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(result), perm); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	return Result{Data: fmt.Sprintf("Edited %s: %d replacement(s)", path, replacements)}, nil
}

func (t *EditTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("edit modifies file content")
}

// ──── Fuzzy fallback for near-miss oldString values ────
//
// This only runs after an exact strings.Count match has already failed
// (count == 0), so it can never change behavior for the common case where
// the model got the text exactly right. It has two tiers:
//
//  1. Normalized, unambiguous match: whitespace-insensitive comparison finds
//     exactly one candidate location → safe to auto-apply, since collapsing
//     whitespace runs and trimming line edges essentially never changes the
//     *meaning* of the located block, only how confidently we could match it
//     byte-for-byte.
//  2. Everything else: never auto-applied. If a reasonably similar block is
//     found (or several equally-normalized candidates, which would be
//     genuinely ambiguous), the real file content and line numbers are
//     reported back so the model can retry with exact text instead of
//     guessing blindly again.
//
// Large-file guard: bails out with no hint (falls back to the plain "not
// found" message) above fuzzyMaxLines, so this can't become a perf cliff on
// huge generated/vendored files.
const fuzzyMaxLines = 20000

func fuzzyApplyEdit(content, oldS, newS string) (result string, applied bool, hint string) {
	oldLines := strings.Split(oldS, "\n")
	includeTrailingNL := false
	if len(oldLines) > 1 && oldLines[len(oldLines)-1] == "" && strings.HasSuffix(oldS, "\n") {
		oldLines = oldLines[:len(oldLines)-1]
		includeTrailingNL = true
	}
	n := len(oldLines)
	if n == 0 {
		return "", false, ""
	}

	normOld := make([]string, n)
	for i, l := range oldLines {
		normOld[i] = normalizeLine(l)
	}

	lines, widths := splitLinesWithWidths(content)
	if len(lines) == 0 || len(lines) > fuzzyMaxLines || n > len(lines) {
		return "", false, ""
	}

	var matchStarts []int
	for start := 0; start+n <= len(lines); start++ {
		match := true
		for k := 0; k < n; k++ {
			if normalizeLine(lines[start+k]) != normOld[k] {
				match = false
				break
			}
		}
		if match {
			matchStarts = append(matchStarts, start)
		}
	}

	if len(matchStarts) == 1 {
		s := matchStarts[0]
		byteStart := 0
		for i := 0; i < s; i++ {
			byteStart += widths[i]
		}
		regionWidth := 0
		for i := s; i < s+n-1; i++ {
			regionWidth += widths[i]
		}
		if includeTrailingNL {
			regionWidth += widths[s+n-1]
		} else {
			regionWidth += len(lines[s+n-1])
		}
		byteEnd := byteStart + regionWidth
		if byteStart < 0 || byteEnd > len(content) || byteStart > byteEnd {
			return "", false, "" // defensive: never slice out of range
		}
		result = content[:byteStart] + newS + content[byteEnd:]
		return result, true, ""
	}

	if len(matchStarts) > 1 {
		return "", false, fmt.Sprintf(
			"Found %d whitespace-normalized matches (ambiguous) at lines %s. Add more surrounding context to oldString to disambiguate.",
			len(matchStarts), formatLineRanges(matchStarts, n))
	}

	// No exact normalized match either. Look for the single most similar
	// window purely to help the model retry — never applied automatically.
	bestStart, bestScore := -1, 0.0
	for start := 0; start+n <= len(lines); start++ {
		score := windowSimilarity(lines[start:start+n], normOld)
		if score > bestScore {
			bestScore = score
			bestStart = start
		}
	}
	if bestStart >= 0 && bestScore >= 0.6 {
		snippet := strings.Join(lines[bestStart:bestStart+n], "\n")
		hint = fmt.Sprintf(
			"Closest candidate (%.0f%% similar) is at lines %d-%d:\n---\n%s\n---\nCompare carefully against oldString (spacing, quotes, identifiers) and retry with the exact text.",
			bestScore*100, bestStart+1, bestStart+n, truncateForHint(snippet, 800))
	}
	return "", false, hint
}

// normalizeLine collapses internal whitespace runs to a single space and
// trims leading/trailing whitespace (this also absorbs a trailing "\r" on
// CRLF files, since strings.Fields treats it as whitespace).
func normalizeLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// splitLinesWithWidths splits content into lines *without* their line
// terminator, plus the exact original byte width of each line *including*
// its terminator (or, for a final unterminated line, just its own length).
// Keeping widths lets callers reconstruct precise byte offsets for a
// replacement without re-scanning the original content.
func splitLinesWithWidths(content string) (lines []string, widths []int) {
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, content[start:i])
			widths = append(widths, i-start+1)
			start = i + 1
		}
	}
	if start < len(content) || len(content) == 0 {
		lines = append(lines, content[start:])
		widths = append(widths, len(content)-start)
	}
	return
}

// windowSimilarity scores how close a candidate block of raw file lines is
// to the (already-normalized) oldString lines, using per-line word-set
// Jaccard similarity averaged across the window. This is intentionally
// cheap (no character-level edit distance) so it stays fast even when
// scanning every window of a several-thousand-line file.
func windowSimilarity(windowLines []string, normOld []string) float64 {
	if len(windowLines) != len(normOld) || len(windowLines) == 0 {
		return 0
	}
	var total float64
	for i := range windowLines {
		total += jaccard(strings.Fields(windowLines[i]), strings.Fields(normOld[i]))
	}
	return total / float64(len(windowLines))
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	setA := make(map[string]bool, len(a))
	for _, w := range a {
		setA[w] = true
	}
	setB := make(map[string]bool, len(b))
	for _, w := range b {
		setB[w] = true
	}
	inter := 0
	for w := range setA {
		if setB[w] {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

func formatLineRanges(starts []int, n int) string {
	parts := make([]string, 0, len(starts))
	for _, s := range starts {
		if n <= 1 {
			parts = append(parts, fmt.Sprintf("%d", s+1))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", s+1, s+n))
		}
	}
	return strings.Join(parts, ", ")
}

func truncateForHint(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
