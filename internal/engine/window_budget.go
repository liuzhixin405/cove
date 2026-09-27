package engine

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/token"
)

// Everything the engine adds to a request on its own (repo map excerpt,
// memories, tool results) is sized to the model's context window. The byte
// caps in context_budget.go and session_memory.go apply in full from
// injectionReferenceWindow up; a smaller window gets a proportional share,
// never below a floor that still carries something useful. A 16K local
// model used to receive the same 12KB repo map excerpt as a 200K model:
// with the system prompt and tool definitions that was more than the whole
// window before the first tool result arrived.
const injectionReferenceWindow = 128000

// windowScale is the fraction of the full injection caps model gets.
func windowScale(model string) float64 {
	w := api.ContextWindowForModel(model)
	if w <= 0 || w >= injectionReferenceWindow {
		return 1
	}
	return float64(w) / float64(injectionReferenceWindow)
}

func scaledBytes(max, floor int, model string) int {
	n := int(float64(max) * windowScale(model))
	if n < floor {
		n = floor
	}
	return n
}

// repoMapExcerptBudget is the byte cap of the <repo_map_excerpt> turn note
// for model.
func repoMapExcerptBudget(model string) int {
	return scaledBytes(repoMapExcerptMaxBytes, 1024, model)
}

// turnMemoryBudget is the byte cap of the whole <turn_memories> note for
// model; relevantMemoryBudget the cap of its retrieved-memories part.
func turnMemoryBudget(model string) int {
	return scaledBytes(turnMemoryNoteMaxBytes, 768, model)
}

func relevantMemoryBudget(model string) int {
	return scaledBytes(relevantMemoryNoteMaxBytes, 512, model)
}

// toolResultBudgetTokens is the most tokens one tool result may occupy in
// model's history: an eighth of the window, between 1000 and 24000. A
// skill's full text or a long file read that takes half the window leaves
// nothing for the work itself.
func toolResultBudgetTokens(model string) int {
	if !api.ContextWindowKnown(model) {
		// A guessed window is no reason to cut results; the per-tool caps
		// (toolOutputLimit) apply as tuned.
		return 24000
	}
	n := api.ContextWindowForModel(model) / 8
	if n < 1000 {
		n = 1000
	}
	if n > 24000 {
		n = 24000
	}
	return n
}

// capToolResult cuts content to toolResultBudgetTokens(model), keeping the
// head and the tail (errors are usually at the end) and ending with a note
// that says how much was cut and why, so the model asks for a narrower
// read instead of assuming it saw everything.
func capToolResult(model, toolName, content string) string {
	budget := toolResultBudgetTokens(model)
	est := token.Estimate(content)
	if est <= budget {
		return content
	}
	// Leave room for the note itself.
	kept := token.TruncateMiddle(content, budget-60)
	return kept + "\n[tool result truncated: about " + itoa(est) + " tokens of " + toolName + " output, " +
		itoa(budget) + " kept; the model " + model + " has a " + itoa(api.ContextWindowForModel(model)) +
		"-token context window. Re-run with a narrower range (offset/limit, a more specific pattern) if the cut part matters.]"
}

// smallWindowTools is the tool set a model with a small context window is
// offered: the file, search, shell and planning tools a coding task needs.
// The full registry's definitions were ~3.9K tokens, a quarter of a 16K
// window; the tools left out (sub-agents, teams, worktrees, cron, browser,
// MCP proxies) are the ones such a model cannot drive well anyway.
var smallWindowTools = map[string]bool{
	"read": true, "read_file": true, "write": true, "edit": true, "multi": true,
	"bash": true, "powershell": true, "glob": true, "grep": true, "search": true,
	"todowrite": true, "question": true, "repo_map": true, "skill_view": true,
	"skills_list": true, "skill": true, "webfetch": true, "lsp": true,
}

// smallWindowLimit is the context window below which only smallWindowTools
// are sent.
const smallWindowLimit = 48000

func smallWindow(model string) bool {
	if !api.ContextWindowKnown(model) {
		return false
	}
	w := api.ContextWindowForModel(model)
	return w > 0 && w < smallWindowLimit
}

// absPathRe finds absolute paths in a request: a Windows drive path or a
// Unix path with at least two components (so "/" alone or "a/b" do not
// count).
var absPathRe = regexp.MustCompile(`(?i)\b[a-z]:[/\\][^\s"'<>|*?，。；：]+|(?:^|\s)/[^\s"'<>|*?，。；：/]+(?:/[^\s"'<>|*?，。；：/]+)+`)

// queryTargetsOtherDirectory reports whether query names an absolute path
// outside root (the repository the repo map describes): the task is about
// another directory, and root's repo map would only be noise.
func queryTargetsOtherDirectory(query, root string) bool {
	root = strings.ToLower(filepath.Clean(root))
	if root == "" || root == "." {
		return false
	}
	for _, m := range absPathRe.FindAllString(query, -1) {
		p := strings.ToLower(filepath.Clean(strings.TrimSpace(m)))
		if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) || strings.HasPrefix(p, root+"/") {
			continue
		}
		return true
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
