package engine

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// readOnlyTools lists tool names that are inherently read-only / non-destructive.
// Repeated calls to these tools should NOT trigger loop detection because
// reading/searching/browsing is legitimate multi-step work.
var readOnlyTools = map[string]bool{
	"read":        true,
	"grep":        true,
	"glob":        true,
	"lsp":         true,
	"webfetch":    true,
	"browser":     true,
	"task_list":   true,
	"skills_list": true,
	"skill_view":  true,
}

// LoopDetector tracks tool-call patterns and output content across
// iterations to detect when the model is stuck in a non-productive loop.
//
// Three-layer detection:
//   - Layer 1a (exact fingerprint): same tool-call set appears >= N times in last window -> loop
//   - Layer 1b (fuzzy / same-tool): same tool name but different params appears >= N times -> loop
//   - Layer 2 (output content hash): same output prefix appears >= N times in window -> loop
//   - Layer 3 (stagnation): after M iterations with no new files created/modified -> loop
//
// Adaptive thresholds: flash (fast) models get more sensitive thresholds
// because they are more prone to repetitive loops.
type LoopDetector struct {
	// Layer 1a: exact tool-call fingerprint ring buffer
	fpHistory []string // most recent N fingerprints
	fpWindow  int      // max history size (default 12)
	fpThresh  int      // break threshold (default 8)

	// Layer 1b: fuzzy (tool-name-only) fingerprint ring buffer
	toolOnlyHistory []string // tool names only, no params
	toolOnlyWindow  int      // max history size (default 12)
	toolOnlyThresh  int      // break threshold (default 10)

	// Layer 1b progress detection: track output diversity per tool pattern.
	// If the same tool is called repeatedly but produces DIFFERENT outputs,
	// it's making progress and should NOT be flagged as a loop.
	// Map: toolOnlyPattern -> []outputHash (ring buffer, window = toolOnlyWindow)
	toolOutputs map[string][]string
	// Map: toolOnlyPattern -> []dirSignature (ring buffer, window = toolOnlyWindow)
	// Helps suppress false positives when the same tool pattern is exploring
	// genuinely different directories/subprojects.
	toolDirs map[string][]string

	// Tracks the most recent tool pattern (set by RecordToolCalls, read by RecordOutput).
	lastToolOnlyPattern string

	// Layer 2: output content hash sliding window
	outHashes []string       // most recent N hashes
	outCounts map[string]int // hash -> count within window
	outWindow int            // window size (default 40)
	outThresh int            // break threshold (default 8)

	// Guard: track how many times we've broken a loop in this turn.
	// On the Nth break we escalate (hard stop).
	breakCount int
	maxBreaks  int // default 5

	// Layer 3: stagnation tracking
	// Tracks whether we're creating/modifying files productively.
	filesCreated map[string]bool // files created this turn
	filesWritten map[string]bool // files written this turn
	stallCount   int             // consecutive iterations without new file activity
	stallThresh  int             // iterations before stagnation detection (default 25)

	// Whether this is a "fast" (flash) model -> uses lower thresholds.
	isFastModel bool
}

// LoopResult describes what was detected.
type LoopResult struct {
	Detected bool
	Reason   string // human-readable
	Layer    int    // 1, 2, or 3
	Fatal    bool   // true -> hard stop, false -> inject guidance
}

// NewLoopDetector returns a detector with sensible defaults.
// Thresholds are set conservatively to avoid false positives during
// legitimate multi-step workflows.
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		fpHistory:       make([]string, 0, 14),
		fpWindow:        14,
		fpThresh:        10,
		toolOnlyHistory: make([]string, 0, 12),
		toolOnlyWindow:  12,
		toolOnlyThresh:  10,
		outHashes:       make([]string, 0, 40),
		outCounts:       make(map[string]int, 40),
		outWindow:       40,
		outThresh:       8,
		maxBreaks:       5,
		filesCreated:    make(map[string]bool),
		filesWritten:    make(map[string]bool),
		stallCount:      0,
		stallThresh:     60, // only flag after 60 iterations without file activity
		isFastModel:     false,
		toolOutputs:     make(map[string][]string),
		toolDirs:        make(map[string][]string),
	}
}

// NewLoopDetectorWithModel creates a detector with thresholds tuned
// for the given model type. Fast (flash) models get tighter thresholds.
func NewLoopDetectorWithModel(isFastModel bool) *LoopDetector {
	ld := NewLoopDetector()
	ld.isFastModel = isFastModel
	if isFastModel {
		// Flash models are more prone to getting stuck -> more sensitive,
		// but still conservative to avoid false positives.
		ld.fpWindow = 12
		ld.fpThresh = 8 // 8/12, relaxed from 6/10 to reduce false positives
		ld.toolOnlyWindow = 10
		ld.toolOnlyThresh = 8
		ld.outWindow = 30
		ld.outThresh = 8    // 8/30
		ld.stallThresh = 50 // more tolerant for flash models
	}
	return ld
}

// isAllReadOnlyTools checks whether ALL tool names in a fingerprint
// are read-only tools. If so, the fingerprint should be exempt from
// loop detection (reading/searching is legitimate multi-step work).
func isAllReadOnlyTools(fp string) bool {
	if fp == "" {
		return false
	}
	parts := strings.Split(fp, "|")
	for _, p := range parts {
		colonIdx := strings.Index(p, ":")
		name := p
		if colonIdx > 0 {
			name = p[:colonIdx]
		}
		if !readOnlyTools[name] {
			return false
		}
	}
	return len(parts) > 0
}

// extractToolPatterns parses a fingerprint (format: "tool:param_value|tool:param_value|...")
// and returns "tool:param_key" entries in sorted order.
// This distinguishes e.g. "bash:command" from "bash:workdir" — same tool,
// different parameter keys means different operation patterns.
// The param key is extracted from the first colon-separated segment after the tool name
// (truncated to the first 20 chars to keep it compact).
func extractToolPatterns(fp string) string {
	if fp == "" {
		return ""
	}
	parts := strings.Split(fp, "|")
	patterns := make([]string, 0, len(parts))
	for _, p := range parts {
		colonIdx := strings.Index(p, ":")
		if colonIdx > 0 {
			toolName := p[:colonIdx]
			// The fingerprint format is "tool:param_value|..." where param_value
			// is the value of the first non-empty well-known key (filePath, command, etc.).
			// We don't have the key name in the fingerprint, so use the tool name + value prefix.
			// This still distinguishes "bash:go build" from "bash:ls -la".
			valPrefix := p[colonIdx+1:]
			if len(valPrefix) > 20 {
				valPrefix = valPrefix[:20]
			}
			patterns = append(patterns, toolName+":"+valPrefix)
		} else {
			patterns = append(patterns, p)
		}
	}
	sort.Strings(patterns)
	return strings.Join(patterns, "|")
}

// RecordToolCalls records a set of tool calls. The fingerprint should
// already be computed by the caller (typically via fingerprintToolCalls).
//
// Detection logic:
//   - Layer 1a: exact fingerprint match in sliding window
//   - Layer 1b: same tool name(s) but different params in sliding window
//
// Read-only tools (read, grep, glob, etc.) are exempt from loop detection
// because repeatedly reading/searching is legitimate multi-step work.
func (ld *LoopDetector) RecordToolCalls(fp string) LoopResult {
	if fp == "" {
		return LoopResult{}
	}

	// Exempt pure read-only tool calls from loop detection entirely.
	// Reading/searching/browsing is legitimate multi-step work and
	// should never be flagged as a loop.
	if isAllReadOnlyTools(fp) {
		return LoopResult{}
	}

	// Layer 1a: Exact fingerprint match
	ld.fpHistory = append(ld.fpHistory, fp)
	if len(ld.fpHistory) > ld.fpWindow {
		ld.fpHistory = ld.fpHistory[1:]
	}

	cnt := ld.countFP(fp, ld.fpWindow)
	if cnt >= ld.fpThresh {
		ld.breakCount++
		reason := fmt.Sprintf(
			"检测到工具调用循环(L1): 相同模式连续出现 %d/%d 次 — 模型可能在原地打转",
			cnt, ld.fpWindow,
		)
		fatal := ld.breakCount >= ld.maxBreaks
		return LoopResult{
			Detected: true,
			Reason:   reason,
			Layer:    1,
			Fatal:    fatal,
		}
	}

	// Layer 1b: Fuzzy (tool-name-only) match
	// Same tool names repeating with different parameters is also a loop signal.
	// Skip if all tools are read-only (already checked above) or if the
	// pattern contains a mix of read + write tools (which is productive work).
	toolOnly := extractToolPatterns(fp)
	if toolOnly != "" {
		ld.toolOnlyHistory = append(ld.toolOnlyHistory, toolOnly)
		if len(ld.toolOnlyHistory) > ld.toolOnlyWindow {
			ld.toolOnlyHistory = ld.toolOnlyHistory[1:]
		}
		if dirSig := extractDirectorySignature(fp); dirSig != "" {
			ld.recordToolDir(toolOnly, dirSig)
			// If one repeated tool pattern is exploring >=3 distinct directories,
			// treat it as likely-progressive work and suppress L1b for this pattern.
			if uniqueCount(ld.toolDirs[toolOnly]) >= 3 {
				ld.clearToolOnlyFromHistory(toolOnly)
				return LoopResult{}
			}
		}

		// Save the current tool pattern so RecordOutput can correlate
		// output diversity later for progress detection.
		ld.lastToolOnlyPattern = toolOnly

		toolCnt := ld.countToolOnly(toolOnly, ld.toolOnlyWindow)
		if toolCnt >= ld.toolOnlyThresh {
			ld.breakCount++
			reason := fmt.Sprintf(
				"检测到工具调用循环(L1b): 相同工具(%s)反复调用 %d/%d 次，参数在变化但模式无进展 — 模型可能卡在无效尝试中",
				toolOnly, toolCnt, ld.toolOnlyWindow,
			)
			fatal := ld.breakCount >= ld.maxBreaks
			return LoopResult{
				Detected: true,
				Reason:   reason,
				Layer:    1,
				Fatal:    fatal,
			}
		}
	}

	return LoopResult{}
}

// RecordOutput records a tool-call output string. Only the first 512 bytes
// are hashed so giant file contents don't mask genuine output-level loops.
//
// Additionally performs Layer 1b progress detection: if the same tool pattern
// produces DIFFERENT outputs each time, it's making progress — retroactively
// clear the Layer 1b history to prevent false positives.
func (ld *LoopDetector) RecordOutput(output string) LoopResult {
	if len(output) == 0 {
		return LoopResult{}
	}
	// "No changes" style outputs are common in idempotent workflows and should
	// not be treated as loop evidence.
	if isNoChangeOutput(output) {
		return LoopResult{}
	}
	h := hashPrefix(output, 512)

	// --- Layer 1b progress detection ---
	// Track output diversity per tool pattern.
	// If one tool pattern produces ≥2 unique output hashes in the window,
	// it's making progress -> clear that pattern from Layer 1b history.
	if ld.lastToolOnlyPattern != "" {
		ld.toolOutputs[ld.lastToolOnlyPattern] = append(
			ld.toolOutputs[ld.lastToolOnlyPattern], h,
		)
		// Trim to window size
		outs := ld.toolOutputs[ld.lastToolOnlyPattern]
		if len(outs) > ld.toolOnlyWindow {
			ld.toolOutputs[ld.lastToolOnlyPattern] = outs[len(outs)-ld.toolOnlyWindow:]
		}
		// Check diversity: if ≥4 unique hashes in window, it's PROGRESS.
		// 2-3 unique outputs could still be a loop (e.g. alternating results);
		// 4+ unique outputs indicates genuine multi-step work.
		unique := make(map[string]bool, len(outs))
		for _, o := range outs {
			unique[o] = true
		}
		if len(unique) >= 4 {
			// Outputs are diverse -> retroactively clear this pattern
			// from Layer 1b history to prevent false positive.
			ld.clearToolOnlyFromHistory(ld.lastToolOnlyPattern)
		}
	}

	// --- Layer 2: output content hash detection ---
	ld.outHashes = append(ld.outHashes, h)
	ld.outCounts[h]++
	if len(ld.outHashes) > ld.outWindow {
		old := ld.outHashes[0]
		ld.outHashes = ld.outHashes[1:]
		ld.outCounts[old]--
		if ld.outCounts[old] <= 0 {
			delete(ld.outCounts, old)
		}
	}

	if ld.outCounts[h] >= ld.outThresh {
		ld.breakCount++
		reason := fmt.Sprintf(
			"检测到输出内容循环(L2): 相同工具输出出现了 %d/%d 次 — 模型可能在无意义重复",
			ld.outCounts[h], ld.outWindow,
		)
		fatal := ld.breakCount >= ld.maxBreaks
		return LoopResult{
			Detected: true,
			Reason:   reason,
			Layer:    2,
			Fatal:    fatal,
		}
	}
	return LoopResult{}
}

// RecordFileActivity records that a file was created or written during this iteration.
// Used by Layer 3 stagnation detection.
func (ld *LoopDetector) RecordFileActivity(filePath string, isCreate bool) {
	if isCreate {
		ld.filesCreated[filePath] = true
	}
	ld.filesWritten[filePath] = true
}

// RecordIteration records one full tool-call iteration and checks for stagnation.
// Layer 3: if many iterations pass without any new file activity, it's a stagnation loop.
func (ld *LoopDetector) RecordIteration() LoopResult {
	ld.stallCount++

	if ld.stallCount >= ld.stallThresh {
		totalCreated := len(ld.filesCreated)
		totalWritten := len(ld.filesWritten)

		if totalCreated == 0 && totalWritten == 0 {
			// Layer 3 is an advisory signal only. Read/search-heavy tasks can
			// legitimately run many iterations without file writes, so this should
			// never escalate to a hard stop.
			reason := fmt.Sprintf(
				"检测到停滞循环(L3): 连续 %d 轮迭代没有任何文件创建或修改 — 模型可能在空转",
				ld.stallCount,
			)
			// Reset after notifying so we emit at most once per stall window
			// instead of spamming every iteration.
			ld.stallCount = 0
			return LoopResult{
				Detected: true,
				Reason:   reason,
				Layer:    3,
				Fatal:    false,
			}
		}
		// Reset stall count periodically so it can detect new stagnation phases
		ld.stallCount = 0
	}

	return LoopResult{}
}

// clearToolOnlyFromHistory removes ALL occurrences of the given tool pattern
// from the Layer 1b history. Used by progress detection: when outputs are
// diverse, we retroactively clear that tool pattern to prevent false positives.
func (ld *LoopDetector) clearToolOnlyFromHistory(pattern string) {
	filtered := make([]string, 0, len(ld.toolOnlyHistory))
	for _, h := range ld.toolOnlyHistory {
		if h != pattern {
			filtered = append(filtered, h)
		}
	}
	ld.toolOnlyHistory = filtered
}

func (ld *LoopDetector) recordToolDir(pattern, dirSig string) {
	if pattern == "" || dirSig == "" {
		return
	}
	ld.toolDirs[pattern] = append(ld.toolDirs[pattern], dirSig)
	if len(ld.toolDirs[pattern]) > ld.toolOnlyWindow {
		ld.toolDirs[pattern] = ld.toolDirs[pattern][len(ld.toolDirs[pattern])-ld.toolOnlyWindow:]
	}
}

func uniqueCount(items []string) int {
	if len(items) == 0 {
		return 0
	}
	uniq := make(map[string]bool, len(items))
	for _, it := range items {
		if it == "" {
			continue
		}
		uniq[it] = true
	}
	return len(uniq)
}

func extractDirectorySignature(fp string) string {
	if fp == "" {
		return ""
	}
	parts := strings.Split(fp, "|")
	dirs := make([]string, 0, len(parts))
	for _, p := range parts {
		idx := strings.Index(p, ":")
		if idx <= 0 || idx+1 >= len(p) {
			continue
		}
		val := strings.TrimSpace(p[idx+1:])
		if d := normalizeDirSig(deriveDirFromValue(val)); d != "" {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return ""
	}
	sort.Strings(dirs)
	return strings.Join(dirs, "|")
}

func deriveDirFromValue(val string) string {
	if val == "" {
		return ""
	}
	v := strings.TrimSpace(val)
	v = strings.Trim(v, `"'`)
	l := strings.ToLower(v)
	if rest, ok := parseDirCommand(v, l); ok {
		for _, sep := range []string{";", "&&", "||"} {
			if idx := strings.Index(rest, sep); idx >= 0 {
				rest = strings.TrimSpace(rest[:idx])
				break
			}
		}
		return rest
	}
	if strings.Contains(v, "\\") || strings.Contains(v, "/") {
		d := filepath.Dir(v)
		if d == "." {
			return ""
		}
		return d
	}
	return ""
}

func parseDirCommand(raw, lower string) (string, bool) {
	type cmdPrefix struct {
		low    string
		rawLen int
	}
	prefixes := []cmdPrefix{
		{low: "cd ", rawLen: len("cd ")},
		{low: "set-location ", rawLen: len("set-location ")},
		{low: "pushd ", rawLen: len("pushd ")},
		{low: "sl ", rawLen: len("sl ")},
	}
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p.low) {
			return strings.TrimSpace(raw[p.rawLen:]), true
		}
	}
	return "", false
}

func normalizeDirSig(dir string) string {
	d := strings.TrimSpace(dir)
	if d == "" {
		return ""
	}
	d = strings.ReplaceAll(d, "\\", "/")
	d = strings.TrimRight(d, "/")
	return strings.ToLower(d)
}

// ResetFingerprintHistory clears only the tool-call fingerprint history
// (both exact and tool-only) without resetting the entire detector.
// Used after injecting loop guidance so the model starts fresh.
func (ld *LoopDetector) ResetFingerprintHistory() {
	ld.fpHistory = ld.fpHistory[:0]
	ld.toolOnlyHistory = ld.toolOnlyHistory[:0]
	ld.toolOutputs = make(map[string][]string)
	ld.toolDirs = make(map[string][]string)
	ld.lastToolOnlyPattern = ""
}

// Reset clears all accumulated history. Call on each new user turn.
func (ld *LoopDetector) Reset() {
	ld.fpHistory = ld.fpHistory[:0]
	ld.toolOnlyHistory = ld.toolOnlyHistory[:0]
	ld.outHashes = ld.outHashes[:0]
	ld.outCounts = make(map[string]int, ld.outWindow)
	ld.breakCount = 0
	ld.filesCreated = make(map[string]bool)
	ld.filesWritten = make(map[string]bool)
	ld.stallCount = 0
	ld.toolOutputs = make(map[string][]string)
	ld.toolDirs = make(map[string][]string)
	ld.lastToolOnlyPattern = ""
}

// countFP returns how many times fp appears in the most recent 'window' entries.
func (ld *LoopDetector) countFP(fp string, window int) int {
	start := len(ld.fpHistory) - window
	if start < 0 {
		start = 0
	}
	n := 0
	for _, h := range ld.fpHistory[start:] {
		if h == fp {
			n++
		}
	}
	return n
}

// countToolOnly returns how many times toolOnly appears in the most recent 'window' entries.
func (ld *LoopDetector) countToolOnly(toolOnly string, window int) int {
	start := len(ld.toolOnlyHistory) - window
	if start < 0 {
		start = 0
	}
	n := 0
	for _, h := range ld.toolOnlyHistory[start:] {
		if h == toolOnly {
			n++
		}
	}
	return n
}

func hashPrefix(s string, maxLen int) string {
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:8])
}

func isNoChangeOutput(output string) bool {
	s := strings.ToLower(strings.TrimSpace(output))
	if s == "" {
		return false
	}
	markers := []string{
		"no changes",
		"no change",
		"unchanged",
		"already up to date",
		"already up-to-date",
		"already contains the exact content",
		"new content is the same as old content",
		"内容未改变",
		"无改动",
		"没有变化",
	}
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// injectLoopGuidance builds the message inserted into the conversation when
// a non-fatal loop is detected.
func injectLoopGuidance(reason string) string {
	return fmt.Sprintf(
		"[系统检测到重复操作循环]\n%s\n\n请立刻停下来审视当前情况。尝试完全不同的方法，如果实在无法推进请向用户说明问题。",
		strings.TrimSpace(reason),
	)
}
