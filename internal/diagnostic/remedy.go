package diagnostic

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// Remedies: the actions the diagnostic layer takes itself when an error is
// reported. They change run-time strategy or session-level parameters and
// tell the user; they never touch code or config files. Each is bound to its
// code in init.

func init() {
	registry[ErrAPIContextLength].Remedy = contextWindowRemedy
	registry[ErrToolArgsInvalid].Remedy = toolArgsRemedy
}

// contextWindowRemedyPatterns are the ways servers name their window in an
// overflow error, most specific first; the capture is the window.
var contextWindowRemedyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`n_ctx"?\s*:\s*(\d+)`),
	regexp.MustCompile(`context size \((\d+) tokens\)`),
	regexp.MustCompile(`maximum context length is (\d+)`),
	regexp.MustCompile(`context length of only (\d+)`),
	regexp.MustCompile(`\d+\s*\+\s*\d+\s*>\s*(\d+)`),
	regexp.MustCompile(`(\d+) maximum`),
}

// MinUsefulContextWindow is the smallest window cove works in at all: with
// the core tool set its system prompt and tool definitions take about 5K
// tokens, and a reply plus some history need room too. Below it the engine
// stops compacting automatically (see Engine.checkAndCompress) and the user
// is told to grow the server's window. 16K is tight but workable since the
// per-turn injections and tool results scale with the window.
const MinUsefulContextWindow = 12000

// parseContextWindow returns the window an overflow error names, or 0.
func parseContextWindow(msg string) int {
	for _, re := range contextWindowRemedyPatterns {
		if m := re.FindStringSubmatch(msg); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// contextWindowRemedy learns a model's real window from the server's error:
// when the error names a window smaller than cove's estimate for the model,
// the estimate is replaced for this session, so compaction triggers where it
// should and the next /continue fits. The user is told how to make it stick.
func contextWindowRemedy(ev RuntimeEvent, rt Runtime) (string, bool) {
	if ev.Model == "" {
		return "", false
	}
	n := parseContextWindow(ev.Message)
	current := api.ContextWindowForModel(ev.Model)
	if n <= 0 || n >= current {
		return "", false
	}
	rt.SetModelContextWindow(ev.Model, n)
	note := fmt.Sprintf("已按服务端返回的 %d token 调整模型 %s 本会话的上下文预算；要固定下来，在 config.json 加 \"context_window\": %d", n, ev.Model, n)
	applied := fmt.Sprintf("窗口 %d → %d", current, n)
	if p, ok := rt.(WindowPersister); ok {
		// Written for the next start too: the same wall was hit four times
		// in one afternoon with the "add it to config.json" hint on screen.
		if err := p.PersistModelContextWindow(ev.Model, n); err == nil {
			note = fmt.Sprintf("已按服务端返回的 %d token 调整模型 %s 的上下文预算，并已写入 config.json（model_context_windows），下次启动直接生效", n, ev.Model)
			applied += "，已写入配置"
		}
	}
	if n < MinUsefulContextWindow {
		note += fmt.Sprintf("。这个窗口太小，cove 的系统提示和工具定义就要占去大半，请把模型上下文调到 %d 以上（如 llama-server -c 32768）", MinUsefulContextWindow)
	}
	rt.Notify(note)
	return applied, true
}

var (
	toolArgsMu     sync.Mutex
	toolArgsCounts = map[string]int{}
)

// toolArgsRemedy counts invalid tool arguments per model and tells the user
// at the third occurrence, then every fifth: one bad call is noise, a
// pattern says the model is not up to tool calling.
func toolArgsRemedy(ev RuntimeEvent, rt Runtime) (string, bool) {
	if ev.Model == "" {
		return "", false
	}
	toolArgsMu.Lock()
	toolArgsCounts[ev.Model]++
	n := toolArgsCounts[ev.Model]
	toolArgsMu.Unlock()
	if n < 3 || (n > 3 && (n-3)%5 != 0) {
		return "", false
	}
	rt.Notify(fmt.Sprintf("模型 %s 已 %d 次输出非法的工具参数，建议换一个模型或降低 temperature", ev.Model, n))
	return fmt.Sprintf("已提示（第 %d 次）", n), true
}

// ResetRemedyState forgets the remedies' counters, for tests and for
// /diagnose archive, which starts a new cycle.
func ResetRemedyState() {
	toolArgsMu.Lock()
	toolArgsCounts = map[string]int{}
	toolArgsMu.Unlock()
}
