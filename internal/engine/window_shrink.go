package engine

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/token"
)

// shrinkForWindow is the last resort when a request is over the model's
// window and compaction found nothing to summarise (a fresh turn: too few
// messages, no assistant boundary). It removes what the engine itself added
// and what is cheapest to lose, until the estimate is at or under target,
// and reports whether anything changed:
//
//  1. the synthetic turn notes (repo map excerpt, memories, environment),
//     which the model can ask for with tools;
//  2. repeated interruption markers, keeping the last;
//  3. tool results, largest first, cut to a quarter of the window's tool
//     result budget with a note saying so.
//
// The user's messages and the assistant's own turns are never touched.
func (e *Engine) shrinkForWindow(target int) bool {
	before := e.totalTokens
	model := e.currentModel()

	kept := e.messages[:0:0]
	lastMarker := -1
	for _, m := range e.messages {
		if m.Synthetic && isTurnNote(m.Content) {
			continue
		}
		if m.Synthetic && isResumeMarker(m.Content) {
			if lastMarker >= 0 {
				kept[lastMarker] = m
				continue
			}
			lastMarker = len(kept)
		}
		kept = append(kept, m)
	}
	e.messages = kept
	e.invalidateUsage()
	e.updateTokenCount()

	if e.totalTokens > target {
		cap := toolResultBudgetTokens(model) / 4
		if cap < 300 {
			cap = 300
		}
		idx := make([]int, 0, len(e.messages))
		for i, m := range e.messages {
			if m.Role == "tool" && token.Estimate(m.Content) > cap {
				idx = append(idx, i)
			}
		}
		sort.Slice(idx, func(a, b int) bool {
			return len(e.messages[idx[a]].Content) > len(e.messages[idx[b]].Content)
		})
		for _, i := range idx {
			m := &e.messages[i]
			est := token.Estimate(m.Content)
			m.Content = token.TruncateMiddle(m.Content, cap) +
				"\n[tool result cut from about " + itoa(est) + " to " + itoa(cap) + " tokens to fit the context window; re-run the tool with a narrower range if the cut part matters.]"
			e.updateTokenCount()
			if e.totalTokens <= target {
				break
			}
		}
	}
	return e.totalTokens < before
}

// isTurnNote reports whether content is a per-turn note the engine attached
// to a user message (turnContextNote): disposable context, not conversation.
func isTurnNote(content string) bool {
	c := strings.TrimSpace(content)
	for _, tag := range []string{"<turn_memories>", "<repo_map_excerpt>", "<environment>", "<session_memories>", "<relevant_memories>"} {
		if strings.HasPrefix(c, tag) {
			return true
		}
	}
	return false
}

// isResumeMarker reports whether content is the "[system: 上一次执行被中断…]"
// note a resumed request starts with.
func isResumeMarker(content string) bool {
	return strings.Contains(content, "上一次执行被中断") || strings.Contains(content, "上一轮任务被中断")
}

// outsideDirRe matches the two refusals of internal/tool/path_security.go
// and captures the path.
var outsideDirRe = regexp.MustCompile(`path (?:outside working directory|on different drive): ([^\n(]+?)\s*(?:\(cwd is on [^)]*\))?\s*(?:\n|$)`)

// outsideDirectoryOf returns the directory a path refusal in a tool result
// names, or "" when result is not such a refusal.
func outsideDirectoryOf(result string) string {
	m := outsideDirRe.FindStringSubmatch(result)
	if m == nil {
		return ""
	}
	p := strings.TrimSpace(m[1])
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// noteOutsideDirectory tells the user, once per directory and session, that
// a tool refused a path outside the working directory and what to do about
// it. The refusal itself only reached the model, which kept trying other
// tools; the person, who could fix it with one command, saw "工具 write
// 失败" and nothing else.
func (e *Engine) noteOutsideDirectory(result string) {
	dir := outsideDirectoryOf(result)
	if dir == "" {
		return
	}
	key := strings.ToLower(dir)
	if e.outsideDirHinted == nil {
		e.outsideDirHinted = map[string]bool{}
	}
	if e.outsideDirHinted[key] {
		return
	}
	e.outsideDirHinted[key] = true
	e.engineOutput("  \x1b[33m⚠ 模型试图访问工作目录之外的路径（" + dir + "）。cove 的文件工具只在 " + e.projectCwd() +
		" 内读写；要在那个目录工作，请输入 /cd " + dir + " 后重发请求。\x1b[0m")
}
