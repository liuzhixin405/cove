package dream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/log"
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
	onFilesTouched func(paths []string) // callback when dream modifies files
}

// NewRunner creates an auto-dream runner. Call ExecuteAutoDream after each turn.
func NewRunner(provider api.Provider, model string, sessionID string) *Runner {
	home, _ := os.UserHomeDir()
	return &Runner{
		provider:       provider,
		model:          model,
		currentSession: sessionID,
		memoryRoot:     filepath.Join(home, ".agentgo", "memory"),
		sessionsDir:    filepath.Join(home, ".agentgo", "sessions"),
	}
}

// SetOnFilesTouched sets a callback that fires when dream modifies memory files.
func (r *Runner) SetOnFilesTouched(fn func(paths []string)) {
	r.onFilesTouched = fn
}

// ExecuteAutoDream checks all gates and runs the dream if conditions are met.
// This should be called at the end of each turn (from session end hook).
func (r *Runner) ExecuteAutoDream(ctx context.Context) {
	if !IsEnabled() {
		return
	}

	cfg := LoadConfig()

	// --- Time gate ---
	lastAt, err := ReadLastConsolidatedAt()
	if err != nil {
		log.Debugf("[autoDream] ReadLastConsolidatedAt failed: %v", err)
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
		log.Debugf("[autoDream] ListSessionsTouchedSince failed: %v", err)
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
		log.Debugf("[autoDream] lock acquire failed: %v", err)
		return
	}
	if !acquired {
		return
	}

	log.Debugf("[autoDream] firing — %.1fh since last, %d sessions to review",
		hoursSince, len(sessionIDs))

	// Run the dream in a background goroutine
	dreamCtx, cancel := context.WithCancel(ctx)
	task := NewTask(len(sessionIDs), priorMtime, cancel)

	go r.runDream(dreamCtx, task, sessionIDs)
}

// runDream executes the memory consolidation agent loop.
func (r *Runner) runDream(ctx context.Context, task *Task, sessionIDs []string) {
	defer func() {
		if task.Status == StatusRunning {
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
			log.Debugf("[autoDream] API error: %v", err)
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
			if r.onFilesTouched != nil && len(task.FilesTouched) > 0 {
				r.onFilesTouched(task.FilesTouched)
			}
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
	if r.onFilesTouched != nil && len(task.FilesTouched) > 0 {
		r.onFilesTouched(task.FilesTouched)
	}
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

	// Block redirects and pipes to files
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
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
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
	if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
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
	// Truncate large files
	content := string(data)
	if len(content) > 30000 {
		content = content[:30000] + "\n... [truncated]"
	}
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

	// Use a simple grep via bash
	cmd := fmt.Sprintf("grep -rn %q %s", pattern, path)
	return executeReadOnlyCommand(cmd)
}

// isInsideMemoryDir checks if the given absolute path is inside the memory directory.
func isInsideMemoryDir(absPath, memRoot string) bool {
	absMemRoot, _ := filepath.Abs(memRoot)
	return strings.HasPrefix(strings.ToLower(filepath.Clean(absPath)),
		strings.ToLower(filepath.Clean(absMemRoot)))
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
