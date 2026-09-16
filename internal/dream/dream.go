package dream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/fsatomic"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/textutil"
)

// scanThrottle: when time-gate passes but session-gate doesn't, avoid scanning every turn.
const scanThrottleInterval = 10 * time.Minute

// Runner manages the auto-dream lifecycle.
type Runner struct {
	mu             sync.Mutex
	lastScanAt     time.Time
	provider       api.Provider
	model          string
	currentSession string
	memoryRoot     string
	sessionsDir    string
}

// NewRunner creates an auto-dream runner. Call ExecuteAutoDream after each turn.
func NewRunner(provider api.Provider, model string, sessionID string) *Runner {
	home, _ := os.UserHomeDir()
	return &Runner{
		provider:       provider,
		model:          model,
		currentSession: sessionID,
		memoryRoot:     filepath.Join(home, ".cove", "memory"),
		sessionsDir:    filepath.Join(home, ".cove", "sessions"),
	}
}

// ExecuteAutoDream checks all gates and runs the dream if conditions are met.
// This should be called at the end of each turn (from session end hook).
const dreamRunTimeout = 5 * time.Minute

// deriveDreamContext returns the context for the background consolidation run.
// It is DETACHED from the parent's cancellation — the turn that triggered the
// dream typically ends (and cancels its context) immediately — while still being
// bounded by its own timeout so a stuck run cannot leak forever.
func deriveDreamContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), dreamRunTimeout)
}

func (r *Runner) ExecuteAutoDream(ctx context.Context) {
	if !IsEnabled() {
		return
	}

	cfg := LoadConfig()

	// --- Time gate ---
	lastAt, err := ReadLastConsolidatedAt()
	if err != nil {
		log.Warnf("[autoDream] ReadLastConsolidatedAt failed: %v", err)
		return
	}

	var hoursSince float64
	if lastAt.IsZero() {
		hoursSince = float64(cfg.MinHours) + 1 // trigger on first run if enough sessions
	} else {
		hoursSince = time.Since(lastAt).Hours()
	}
	if hoursSince < float64(cfg.MinHours) {
		return
	}

	// --- Scan throttle ---
	r.mu.Lock()
	sinceScan := time.Since(r.lastScanAt)
	if sinceScan < scanThrottleInterval {
		r.mu.Unlock()
		log.Debugf("[autoDream] scan throttle — last scan was %ds ago", int(sinceScan.Seconds()))
		return
	}
	r.lastScanAt = time.Now()
	r.mu.Unlock()

	// --- Session gate ---
	sessionIDs, err := ListSessionsTouchedSince(lastAt, r.sessionsDir)
	if err != nil {
		log.Warnf("[autoDream] ListSessionsTouchedSince failed: %v", err)
		return
	}

	// Exclude current session
	filtered := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if id != r.currentSession {
			filtered = append(filtered, id)
		}
	}
	sessionIDs = filtered

	if len(sessionIDs) < cfg.MinSessions {
		log.Debugf("[autoDream] skip — %d sessions since last consolidation, need %d",
			len(sessionIDs), cfg.MinSessions)
		return
	}

	// --- Lock ---
	priorMtime, acquired, err := TryAcquireConsolidationLock()
	if err != nil {
		log.Warnf("[autoDream] lock acquire failed: %v", err)
		return
	}
	if !acquired {
		return
	}

	log.Debugf("[autoDream] firing — %.1fh since last, %d sessions to review",
		hoursSince, len(sessionIDs))

	// Run the dream in a detached background goroutine. Its context must outlive
	// the caller's (the triggering turn ends and cancels its context right away),
	// so derive a detached, self-timed context here. cancel() runs when runDream
	// returns to release the timer; task.CancelFunc still allows explicit early
	// cancellation.
	dreamCtx, cancel := deriveDreamContext(ctx)
	task := NewTask(len(sessionIDs), priorMtime, cancel)

	go func() {
		defer cancel()
		r.runDream(dreamCtx, task, sessionIDs)
	}()
}

// runDream executes the memory consolidation agent loop.
func (r *Runner) runDream(ctx context.Context, task *Task, sessionIDs []string) {
	defer func() {
		if task.CurrentStatus() == StatusRunning {
			task.Fail()
			RollbackConsolidationLock(task.PriorMtime)
		}
	}()

	prompt := BuildConsolidationPrompt(r.memoryRoot, r.sessionsDir, sessionIDs)

	messages := []api.Message{
		{Role: "user", Content: prompt},
	}

	systemPrompt := r.buildDreamSystemPrompt()
	toolDefs := r.buildDreamToolDefs()

	// Run up to 30 iterations (the dream agent should finish well before this)
	const maxDreamIterations = 30
	for iter := 0; iter < maxDreamIterations; iter++ {
		select {
		case <-ctx.Done():
			log.Debugf("[autoDream] cancelled")
			return
		default:
		}

		req := api.ChatRequest{
			Model:      r.model,
			Messages:   messages,
			SystemBase: systemPrompt,
			Tools:      toolDefs,
			MaxTokens:  16000,
		}

		resp, err := r.provider.Chat(ctx, req)
		if err != nil {
			log.Warnf("[autoDream] API error: %v", err)
			task.Fail()
			RollbackConsolidationLock(task.PriorMtime)
			return
		}

		// Track assistant turn
		var touchedPaths []string
		toolUseCount := len(resp.ToolCalls)
		for _, tc := range resp.ToolCalls {
			if tc.Name == "write" || tc.Name == "edit" {
				if fp, ok := tc.Input["filePath"].(string); ok {
					touchedPaths = append(touchedPaths, fp)
				}
			}
		}
		task.AddTurn(Turn{Text: resp.Content, ToolUseCount: toolUseCount}, touchedPaths)

		// No tool calls — dream is done
		if len(resp.ToolCalls) == 0 {
			task.Complete()
			log.Debugf("[autoDream] completed — %d files touched", len(task.FilesTouched))
			return
		}

		// Append assistant message
		messages = append(messages, api.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute tools (restricted to read-only bash + memory file writes)
		for _, tc := range resp.ToolCalls {
			result := r.executeDreamTool(tc)
			messages = append(messages, api.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    result,
			})
		}
	}

	task.Complete()
	log.Debugf("[autoDream] completed (max iterations) — %d files touched", len(task.FilesTouched))
}

// executeDreamTool runs a tool call with dream-mode restrictions.
func (r *Runner) executeDreamTool(tc api.ToolCall) string {
	switch tc.Name {
	case "bash":
		return r.executeDreamBash(tc)
	case "write":
		return r.executeDreamWrite(tc)
	case "edit":
		return r.executeDreamEdit(tc)
	case "read":
		return r.executeDreamRead(tc)
	case "glob":
		return r.executeDreamGlob(tc)
	case "grep":
		return r.executeDreamGrep(tc)
	default:
		return fmt.Sprintf("Error: tool %q is not available in dream mode", tc.Name)
	}
}

// executeDreamBash restricts bash to read-only commands.
func (r *Runner) executeDreamBash(tc api.ToolCall) string {
	cmd, _ := tc.Input["command"].(string)
	if cmd == "" {
		return "Error: empty command"
	}

	// Allow only read-only commands
	allowedPrefixes := []string{
		"ls", "find", "grep", "cat", "stat", "wc", "head", "tail",
		"dir", "type", "findstr", // Windows equivalents
	}
	cmdLower := strings.TrimSpace(strings.ToLower(cmd))
	allowed := false
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(cmdLower, prefix+" ") || cmdLower == prefix {
			allowed = true
			break
		}
	}

	// Block shell meta operators to prevent command chaining/substitution.
	// Dream mode allows only a single read-only command.
	if strings.ContainsAny(cmd, "|;&`$<") || strings.Contains(cmd, "\n") || strings.Contains(cmd, "\r") {
		allowed = false
	}

	// Block output/input redirection.
	if strings.Contains(cmd, ">") || strings.Contains(cmd, ">>") {
		allowed = false
	}

	if !allowed {
		return "Error: only read-only commands (ls, find, grep, cat, stat, wc, head, tail) are allowed in dream mode"
	}

	// Execute via os/exec
	return executeReadOnlyCommand(cmd)
}

// executeDreamWrite writes content to memory files only.
func (r *Runner) executeDreamWrite(tc api.ToolCall) string {
	filePath, _ := tc.Input["filePath"].(string)
	content, _ := tc.Input["content"].(string)

	if filePath == "" || content == "" {
		return "Error: filePath and content are required"
	}

	// Restrict writes to memory directory only
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Sprintf("Error: invalid path: %v", err)
	}
	if !isInsideMemoryDir(absPath, r.memoryRoot) {
		return fmt.Sprintf("Error: dream mode can only write to memory directory (%s)", r.memoryRoot)
	}

	os.MkdirAll(filepath.Dir(absPath), 0700)
	// Atomic replace: a crash mid-write would otherwise leave a half-written
	// memory file, which is then loaded as a memory entry on every later run.
	if err := fsatomic.WriteFile(absPath, []byte(content), 0644); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Written %d bytes to %s", len(content), filePath)
}

// executeDreamEdit edits content in memory files only.
func (r *Runner) executeDreamEdit(tc api.ToolCall) string {
	filePath, _ := tc.Input["filePath"].(string)
	oldStr, _ := tc.Input["oldString"].(string)
	newStr, _ := tc.Input["newString"].(string)

	if filePath == "" || oldStr == "" {
		return "Error: filePath and oldString are required"
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Sprintf("Error: invalid path: %v", err)
	}
	if !isInsideMemoryDir(absPath, r.memoryRoot) {
		return fmt.Sprintf("Error: dream mode can only edit files in memory directory (%s)", r.memoryRoot)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, oldStr) {
		return "Error: oldString not found in file"
	}

	newContent := strings.Replace(content, oldStr, newStr, 1)
	if err := fsatomic.WriteFile(absPath, []byte(newContent), 0644); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Edited %s", filePath)
}

// executeDreamRead reads any file (no restrictions on reading).
func (r *Runner) executeDreamRead(tc api.ToolCall) string {
	filePath, _ := tc.Input["filePath"].(string)
	if filePath == "" {
		return "Error: filePath is required"
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// Truncate large files on a rune boundary: memory files are largely
	// Chinese, so a byte slice at 30000 would cut a rune in half and hand the
	// model invalid UTF-8.
	content := textutil.ClipBytes(string(data), 30000, "\n... [truncated]")
	return content
}

// executeDreamGlob lists files matching a pattern.
func (r *Runner) executeDreamGlob(tc api.ToolCall) string {
	pattern, _ := tc.Input["pattern"].(string)
	if pattern == "" {
		return "Error: pattern is required"
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if len(matches) == 0 {
		return "No matches found"
	}
	return strings.Join(matches, "\n")
}

// executeDreamGrep searches files for a pattern.
func (r *Runner) executeDreamGrep(tc api.ToolCall) string {
	pattern, _ := tc.Input["pattern"].(string)
	path, _ := tc.Input["path"].(string)
	if pattern == "" {
		return "Error: pattern is required"
	}
	if path == "" {
		path = r.memoryRoot
	}

	// Native, shell-free search. AI-generated pattern/path are never passed to a
	// shell, which eliminates the command-injection vector present in the former
	// `sh -c "grep ..."` implementation: %q is Go's quoting, not shell escaping,
	// so a pattern like `$(rm -rf ~)` executed inside the double quotes.
	return grepFiles(pattern, path)
}

// isInsideMemoryDir checks if the given absolute path is inside the memory directory.
//
// The comparison must be on whole path segments. A plain string prefix check
// let any sibling directory whose name merely starts with the memory root
// through — with the root at ~/.cove/memory, the path ~/.cove/memory-evil/x
// passed, so the dream agent's write/edit sandbox could be stepped out of by
// naming a directory carefully.
func isInsideMemoryDir(absPath, memRoot string) bool {
	absMemRoot, err := filepath.Abs(memRoot)
	if err != nil {
		return false
	}
	root := strings.ToLower(filepath.Clean(absMemRoot))
	target := strings.ToLower(filepath.Clean(absPath))
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(os.PathSeparator))
}

// buildDreamSystemPrompt returns a minimal system prompt for the dream agent.
func (r *Runner) buildDreamSystemPrompt() string {
	return `You are a memory consolidation agent. Your job is to organize and improve memory files based on recent session history.

Available tools:
- bash: Run read-only shell commands (ls, find, grep, cat, stat, wc, head, tail only)
- read: Read file contents
- write: Write/create files (memory directory only)
- edit: Edit files (memory directory only)
- glob: List files matching a pattern
- grep: Search files for a pattern

Be concise and efficient. Focus on:
1. Reading existing memory files to understand current state
2. Checking session transcripts for new important information
3. Updating/creating memory files with new knowledge
4. Keeping the INDEX.md file organized and under 200 lines`
}

// buildDreamToolDefs returns tool definitions available to the dream agent.
func (r *Runner) buildDreamToolDefs() []api.ToolDef {
	return []api.ToolDef{
		{
			Name:        "bash",
			Description: "Run a read-only shell command (ls, find, grep, cat, stat, wc, head, tail only)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "The command to run"},
				},
				"required": []any{"command"},
			},
		},
		{
			Name:        "read",
			Description: "Read the contents of a file",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{"type": "string", "description": "Path to the file"},
				},
				"required": []any{"filePath"},
			},
		},
		{
			Name:        "write",
			Description: "Write content to a file (memory directory only)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{"type": "string", "description": "Path to write"},
					"content":  map[string]any{"type": "string", "description": "Content to write"},
				},
				"required": []any{"filePath", "content"},
			},
		},
		{
			Name:        "edit",
			Description: "Edit a file by replacing text (memory directory only)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath":  map[string]any{"type": "string", "description": "Path to edit"},
					"oldString": map[string]any{"type": "string", "description": "Text to replace"},
					"newString": map[string]any{"type": "string", "description": "Replacement text"},
				},
				"required": []any{"filePath", "oldString", "newString"},
			},
		},
		{
			Name:        "glob",
			Description: "List files matching a glob pattern",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Glob pattern"},
				},
				"required": []any{"pattern"},
			},
		},
		{
			Name:        "grep",
			Description: "Search files for a text pattern",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Search pattern"},
					"path":    map[string]any{"type": "string", "description": "Directory to search"},
				},
				"required": []any{"pattern"},
			},
		},
	}
}
