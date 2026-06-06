package onboarding

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Hint identifiers for progressive onboarding.
const (
	HintInterrupt    = "interrupt_usage"      // first Ctrl+C
	HintToolProgress = "tool_progress"        // first tool execution
	HintCompact      = "compact_explanation"  // first context compaction
	HintBudget       = "budget_warning"       // first budget approach
	HintPermission   = "permission_modes"     // first permission ask
	HintMemory       = "memory_system"        // first long session
	HintCheckpoint   = "checkpoint_available" // first file write
)

// Hint messages (Chinese)
var hintMessages = map[string]string{
	HintInterrupt:    "💡 提示：按 Ctrl+C 可以中断当前操作。用 /stop 停止工具执行，/exit 退出程序。",
	HintToolProgress: "💡 提示：工具执行时会显示进度。你可以用 /mode auto 自动批准安全操作。",
	HintCompact:      "💡 提示：对话过长时会自动压缩上下文。用 /compact 可手动触发。",
	HintBudget:       "💡 提示：接近预算上限。用 /budget <金额> 调整，或 /cost 查看详情。",
	HintPermission:   "💡 提示：权限模式可选：default(每次询问) / auto(自动批准) / plan(只规划不执行)。用 /mode 切换。",
	HintMemory:       "💡 提示：我会自动记住重要的偏好和工作模式。用 /memory 查看已学习的内容。",
	HintCheckpoint:   "💡 提示：文件修改前会自动创建检查点。用 /undo 可回退到修改前的状态。",
}

// Hints manages progressive onboarding state.
type Hints struct {
	mu   sync.Mutex
	seen map[string]bool
	path string
}

// NewHints loads or creates the onboarding hints state.
func NewHints() *Hints {
	h := &Hints{seen: make(map[string]bool)}

	dir, err := configDir()
	if err != nil {
		return h
	}
	h.path = filepath.Join(dir, "onboarding.json")

	data, err := os.ReadFile(h.path)
	if err == nil {
		json.Unmarshal(data, &h.seen)
	}
	return h
}

// Show returns the hint message if it hasn't been shown before.
// Returns empty string if already seen. Marks as seen on first call.
func (h *Hints) Show(hint string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.seen[hint] {
		return ""
	}

	h.seen[hint] = true
	h.save()

	msg, ok := hintMessages[hint]
	if !ok {
		return ""
	}
	return fmt.Sprintf("\n  \x1b[33m%s\x1b[0m\n", msg)
}

// HasSeen returns whether a hint was already shown.
func (h *Hints) HasSeen(hint string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[hint]
}

// Reset clears all seen hints (for testing).
func (h *Hints) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = make(map[string]bool)
	h.save()
}

func (h *Hints) save() {
	if h.path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(h.path), 0700)
	data, _ := json.Marshal(h.seen)
	os.WriteFile(h.path, data, 0600)
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cove"), nil
}
