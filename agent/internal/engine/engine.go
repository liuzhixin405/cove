package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentgo/internal/api"
	ctxt "github.com/agentgo/internal/context"
	"github.com/agentgo/internal/cost"
	"github.com/agentgo/internal/hooks"
	"github.com/agentgo/internal/log"
	"github.com/agentgo/internal/memory"
	"github.com/agentgo/internal/permission"
	"github.com/agentgo/internal/repl"
	"github.com/agentgo/internal/session"
	"github.com/agentgo/internal/skills"
	"github.com/agentgo/internal/token"
	"github.com/agentgo/internal/tool"
)

const MaxIterations = 200
const CompactTokenThreshold = 64000

type Config struct {
	Model          string
	PermissionMode string
	MaxBudget      float64
	Debug          bool
	Tools          []tool.Tool
	Provider       api.ProviderConfig
	MemoryStore    *memory.Store
	SkillManager   *skills.Manager
	HookManager    *hooks.Manager
	Classifier     *permission.Classifier
}

type Engine struct {
	provider          api.Provider
	registry          *tool.Registry
	messages          []api.Message
	config            Config
	projCtx           *ctxt.ProjectContext
	costTracker       *cost.Tracker
	perm              *permission.Manager
	store             *session.Store
	session           *session.Record
	memStore          *memory.Store
	skillMgr          *skills.Manager
	hookMgr           *hooks.Manager
	classifier        *permission.Classifier
	systemPrompt      string
	systemOverride    string
	totalTokens       int
	runtime           *tool.Runtime
	fileHistory       map[string]bool
	fileMu            sync.Mutex
	cachedToolDefs    []api.ToolDef
	lastSaveTime      time.Time
	consecutiveErrors int // track consecutive tool failures for circuit breaking
	PermissionPrompt  func(toolName string, input map[string]any, reason string) bool
}

func New(config Config) (*Engine, error) {
	reg := tool.NewRegistry()
	for _, t := range config.Tools {
		reg.Register(t)
	}
	prov := api.DetectProvider(config.Model, config.Provider)
	tracker := cost.NewTracker(config.MaxBudget)
	perm := permission.NewManager(permission.Default)
	if permission.ValidMode(permission.Mode(config.PermissionMode)) {
		perm.SetMode(permission.Mode(config.PermissionMode))
	}
	perm.SetBypassAvailable(true)
	store, _ := session.NewStore()

	e := &Engine{
		provider:    prov,
		registry:    reg,
		messages:    make([]api.Message, 0),
		config:      config,
		costTracker: tracker,
		perm:        perm,
		store:       store,
		memStore:    config.MemoryStore,
		skillMgr:    config.SkillManager,
		hookMgr:     config.HookManager,
		classifier:  config.Classifier,
		runtime: &tool.Runtime{
			Tasks:         make(map[string]*tool.TaskRecord),
			Teams:         make(map[string]*tool.TeamRecord),
			CronSchedules: make(map[string]*tool.CronRecord),
			Messages:      make([]tool.MessageRecord, 0),
			SkillManager:  config.SkillManager,
			SkillPrompts:  make(map[string]string),
		},
		fileHistory: make(map[string]bool),
	}

	if config.SkillManager != nil {
		for _, s := range config.SkillManager.All() {
			e.runtime.SkillPrompts[s.Name] = s.Prompt
		}
	}

	if store != nil {
		e.session = &session.Record{
			ID:        fmt.Sprintf("session-%d", time.Now().Unix()),
			CreatedAt: time.Now(),
			Title:     "New session",
			Model:     config.Model,
		}
	}

	return e, nil
}

func (e *Engine) SetProjectContext(pc *ctxt.ProjectContext) { e.projCtx = pc }
func (e *Engine) SetSystemOverride(prompt string)           { e.systemOverride = prompt }
func (e *Engine) ReloadProvider(provider, model, baseURL, apiKey string) error {
	cfg := api.ProviderConfig{Name: provider, APIKey: apiKey, BaseURL: baseURL}
	prov := api.DetectProvider(model, cfg)
	if err := prov.Validate(); err != nil {
		return err
	}
	e.provider = prov
	e.config.Provider = cfg
	e.config.Model = model
	if e.session != nil {
		e.session.Model = model
	}
	return nil
}
func (e *Engine) Store() *session.Store      { return e.store }
func (e *Engine) Session() *session.Record   { return e.session }
func (e *Engine) CostTracker() *cost.Tracker { return e.costTracker }
func (e *Engine) ProviderName() string       { return e.provider.DisplayName() }
func (e *Engine) SetPermissionMode(mode permission.Mode) {
	if permission.ValidMode(mode) {
		e.perm.SetMode(mode)
		e.config.PermissionMode = string(mode)
	}
}

func (e *Engine) AddPermissionRule(decision permission.Decision, rule permission.Rule) {
	e.perm.AddRule(decision, rule)
}

func (e *Engine) Registry() *tool.Registry { return e.registry }
func (e *Engine) Runtime() *tool.Runtime   { return e.runtime }
func (e *Engine) FileHistory() []string {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	var files []string
	for f := range e.fileHistory {
		files = append(files, f)
	}
	return files
}

func (e *Engine) SystemPrompt() string {
	if e.systemOverride != "" {
		return e.systemOverride
	}
	// Return cached if already built (stable within a session unless context changes)
	if e.systemPrompt != "" {
		return e.systemPrompt
	}
	var sb strings.Builder
	sb.WriteString(`You are an AI coding assistant. You MUST use tools to complete user tasks. Never describe what you would do — actually DO it.

RULES:
1. Use tools for ALL file ops, command execution, code search, web access.
2. Single-step tasks: use the tool immediately, no explanation needed.
3. Multi-step tasks: use todowrite to track progress.
4. Be concise. Use tools to act, not to describe actions.
5. For git, tests, builds — use bash. For files — write/read/edit.
6. Use webfetch for URLs. Use grep/glob for searching code.
7. For creating or fully rewriting files (especially large ones like HTML/CSS/JS): use write with the COMPLETE content in ONE call. Do NOT use many small edit calls for new files.
8. Each tool call response is ONE file operation. Do NOT attempt to write multiple large files in a single response — write them one at a time across iterations.

Available tools:`)
	for _, t := range e.registry.All() {
		d := t.Def()
		sb.WriteString(fmt.Sprintf("\n- %s: %s", d.Name, d.Description))
	}

	if e.skillMgr != nil {
		if sp := e.skillMgr.BuildPrompt(); sp != "" {
			sb.WriteString(sp)
		}
	}

	sb.WriteString("\n\nBe concise. Use tools, not talk.")

	if e.memStore != nil {
		if mp := e.memStore.BuildPrompt(); mp != "" {
			sb.WriteString(mp)
		}
	}

	if e.projCtx != nil {
		sb.WriteString(fmt.Sprintf("\n\nWorking directory: %s | Platform: %s | Shell: %s",
			e.projCtx.Cwd, e.projCtx.Platform, e.projCtx.Shell))
		if e.projCtx.IsGitRepo {
			sb.WriteString(fmt.Sprintf("\nGit: %s (%s)", e.projCtx.GitBranch, e.projCtx.GitStatus))
			if e.projCtx.GitLog != "" {
				sb.WriteString(fmt.Sprintf("\nRecent commits:\n%s", e.projCtx.GitLog))
			}
		}
		if e.projCtx.FileTree != "" {
			sb.WriteString(fmt.Sprintf("\nProject structure:\n%s", e.projCtx.FileTree))
		}
	}

	e.systemPrompt = sb.String()
	return e.systemPrompt
}

func (e *Engine) Run(ctx context.Context, userMessage string) (string, error) {
	return e.RunWithStream(ctx, userMessage, nil)
}

func (e *Engine) RunWithStream(ctx context.Context, userMessage string, onDelta func(delta string)) (string, error) {
	if e.costTracker.OverBudget() {
		return "", fmt.Errorf("budget exceeded: %s", e.costTracker.Summary())
	}

	e.messages = append(e.messages, api.Message{Role: "user", Content: userMessage})
	e.saveSession()

	// Cache system prompt and tool defs across iterations (stable within a run)
	sp := e.SystemPrompt()
	toolDefs := e.buildAPIToolDefs()

	for iter := 0; iter < MaxIterations; iter++ {
		log.Debugf("agent iter=%d msgs=%d tokens=%d tools=%d model=%s cost=%s",
			iter, len(e.messages), e.totalTokens, len(toolDefs), e.config.Model, e.costTracker.Summary())

		req := api.ChatRequest{
			Model:      e.config.Model,
			Messages:   e.messages,
			SystemBase: sp,
			Tools:      toolDefs,
			MaxTokens:  64000,
		}

		var resp *api.ChatResponse
		var err error
		useStream := onDelta != nil

		// Show walking indicator while waiting for API (iter > 0; first call uses main spinner)
		var walker *repl.WalkingIndicator
		if iter > 0 && !e.config.Debug {
			walker = repl.NewWalkingIndicator("Thinking...")
			walker.Start()
		}

		if useStream {
			firstDelta := true
			resp, err = e.provider.ChatStream(ctx, req, func(ev api.StreamEvent) {
				if firstDelta && walker != nil {
					walker.Stop()
					walker = nil
					firstDelta = false
				}
				if ev.Type == "delta" && onDelta != nil {
					onDelta(ev.Delta)
				}
			})
		} else {
			resp, err = e.provider.Chat(ctx, req)
		}

		if walker != nil {
			walker.Stop()
		}

		if err != nil {
			return "", fmt.Errorf("api: %w", err)
		}

		e.costTracker.AddDetailed(e.config.Model, resp.InputTokens, resp.OutputTokens, resp.PromptCacheHitTokens, resp.PromptCacheMissTokens)

		log.Debugf("agent text=%d tools=%d in=%d out=%d stop=%s",
			len(resp.Content), len(resp.ToolCalls), resp.InputTokens, resp.OutputTokens, resp.StopReason)

		// If response was truncated and no complete tool calls survived, ask model to continue
		if (resp.StopReason == "max_tokens" || resp.StopReason == "length") && len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				e.messages = append(e.messages, api.Message{Role: "assistant", Content: resp.Content})
			}
			e.messages = append(e.messages, api.Message{Role: "user", Content: "[system: your previous response was truncated due to length. Please continue, writing one file at a time.]"})
			continue
		}

		if len(resp.ToolCalls) == 0 {
			e.messages = append(e.messages, api.Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent})
			e.saveSession()
			return resp.Content, nil
		}

		assistantMsg := api.Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
		e.messages = append(e.messages, assistantMsg)

		type toolResult struct {
			ID      string
			Name    string
			Content string
		}
		results := make([]toolResult, len(resp.ToolCalls))

		if len(resp.ToolCalls) > 1 {
			var wg sync.WaitGroup
			for i, tc := range resp.ToolCalls {
				t, _ := e.registry.Find(tc.Name)
				safe := t != nil && t.Def().IsConcurrencySafe
				if safe {
					wg.Add(1)
					go func(idx int, tcall api.ToolCall) {
						defer wg.Done()
						res := e.executeTool(ctx, tcall)
						results[idx] = toolResult{ID: tcall.ID, Name: tcall.Name, Content: res}
					}(i, tc)
				} else {
					res := e.executeTool(ctx, tc)
					results[i] = toolResult{ID: tc.ID, Name: tc.Name, Content: res}
				}
			}
			wg.Wait()
		} else {
			for i, tc := range resp.ToolCalls {
				if !e.config.Debug {
					// Show tool start
					fmt.Fprintf(os.Stderr, "\r  \x1b[2m⏳ [%s]...\x1b[0m", tc.Name)
				}
				res := e.executeTool(ctx, tc)
				results[i] = toolResult{ID: tc.ID, Name: tc.Name, Content: res}
			}
		}

		for _, r := range results {
			if !e.config.Debug {
				isErr := strings.HasPrefix(r.Content, "Error:")
				fmt.Fprintf(os.Stderr, "\r\x1b[K%s\n", formatToolLine(r.Name, summarizeResult(r.Content), isErr))
			}
			e.messages = append(e.messages, api.Message{
				Role: "tool", ToolCallID: r.ID, Name: r.Name, Content: r.Content,
			})
		}

		// Circuit breaker: if tools keep failing, hint the model to change approach
		allFailed := true
		for _, r := range results {
			if !strings.HasPrefix(r.Content, "Error:") {
				allFailed = false
				break
			}
		}
		if allFailed && len(results) > 0 {
			e.consecutiveErrors++
			if e.consecutiveErrors >= 3 {
				e.messages = append(e.messages, api.Message{
					Role:    "user",
					Content: "[system: The last 3+ tool calls all failed. Please try a different approach or ask the user for clarification. Do not repeat the same failing pattern.]",
				})
				e.consecutiveErrors = 0
			}
		} else {
			e.consecutiveErrors = 0
		}

		e.totalTokens = countTokens(e.messages)
		if e.totalTokens > CompactTokenThreshold && iter > 5 {
			e.Compact(ctx)
		}
	}

	return "", fmt.Errorf("max iterations (%d) reached, cost: %s", MaxIterations, e.costTracker.Summary())
}

func (e *Engine) executeTool(ctx context.Context, tc api.ToolCall) string {
	t, ok := e.registry.Find(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}

	cwd := ""
	if e.projCtx != nil {
		cwd = e.projCtx.Cwd
	}
	tctx := tool.Context{
		Cwd:              cwd,
		ToolUseID:        tc.ID,
		PermissionMode:   toolPermissionMode(e.perm.Mode()),
		IsNonInteractive: true,
		Debug:            e.config.Debug,
		Runtime:          e.runtime,
	}

	if e.classifier != nil && tc.Name == "bash" {
		cmd, _ := tc.Input["command"].(string)
		cat := e.classifier.Classify(cmd)
		if cat == permission.CatDangerous {
			return fmt.Sprintf("Error: dangerous command blocked: %s", cmd)
		}
		if e.perm.Mode() == permission.Auto && e.classifier.ShouldAutoApprove(cmd) {
			tctx.PermissionMode = "auto"
		}
	}

	if errMsg := t.Validate(tc.Input); errMsg != "" {
		return fmt.Sprintf("Error: invalid %s input: %s", tc.Name, errMsg)
	}

	toolDecision := t.CheckPermissions(tc.Input, tctx)
	decision, reason := e.perm.Check(tc.Name, tc.Input, mapToolDecision(toolDecision.Decision))
	if decision != permission.DAllow && decision != permission.DBypass {
		if reason == "" {
			reason = toolDecision.Reason
		}
		if reason == "" {
			reason = "permission denied"
		}
		if decision == permission.DAsk {
			if e.PermissionPrompt != nil {
				if e.PermissionPrompt(tc.Name, tc.Input, reason) {
					// User approved, continue execution
					goto executeCall
				}
			}
			return fmt.Sprintf("Error: permission denied for %s: user rejected", tc.Name)
		}
		return fmt.Sprintf("Error: permission denied for %s: %s", tc.Name, reason)
	}

executeCall:
	result, err := t.Call(ctx, tc.Input, tctx)
	if err != nil {
		// Retry once for transient errors (network, timeout, temporary file locks)
		if isTransientError(err) {
			time.Sleep(500 * time.Millisecond)
			result, err = t.Call(ctx, tc.Input, tctx)
			if err != nil {
				return fmt.Sprintf("Error (after retry): %v", err)
			}
		} else {
			return fmt.Sprintf("Error: %v", err)
		}
	}
	if !result.IsError {
		e.trackFileChanges(tc)
	}
	output := result.Data
	if !result.IsError {
		// Adaptive truncation: code/read results get more space than bash output
		maxTokens := 4000
		switch tc.Name {
		case "read", "grep":
			maxTokens = 6000 // source code context is more valuable
		case "bash":
			maxTokens = 3000 // build/test output is usually repetitive
		case "webfetch":
			maxTokens = 3000
		}
		output = token.TruncateToTokens(output, maxTokens)
	}
	if result.IsError {
		return result.Data
	}
	return output
}

// isTransientError checks if an error is likely transient and worth retrying
func isTransientError(err error) bool {
	msg := err.Error()
	transientPatterns := []string{
		"timeout", "connection refused", "connection reset",
		"temporary failure", "i/o timeout", "TLS handshake",
		"access is denied", // Windows file locks
		"being used by another process",
	}
	lower := strings.ToLower(msg)
	for _, p := range transientPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func toolPermissionMode(mode permission.Mode) string {
	switch mode {
	case permission.Bypass, permission.Plan:
		return string(mode)
	default:
		return string(permission.Default)
	}
}

func mapToolDecision(decision tool.PermissionResult) permission.Decision {
	switch decision {
	case tool.Allow:
		return permission.DAllow
	case tool.Deny:
		return permission.DDeny
	case tool.Bypass:
		return permission.DBypass
	default:
		return permission.DAsk
	}
}

func (e *Engine) trackFileChanges(tc api.ToolCall) {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	switch tc.Name {
	case "write", "edit":
		if path, ok := tc.Input["filePath"].(string); ok {
			e.fileHistory[path] = true
		}
	case "bash":
		if e.projCtx == nil {
			return
		}
		if cmd, ok := tc.Input["command"].(string); ok {
			for _, word := range strings.Fields(cmd) {
				if strings.Contains(word, ".") && !strings.HasPrefix(word, "-") {
					if f, _ := resolvePath(word, e.projCtx.Cwd); f != "" {
						if _, err := os.Stat(f); err == nil {
							e.fileHistory[f] = true
						}
					}
				}
			}
		}
	}
}

func resolvePath(p, cwd string) (string, bool) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), true
	}
	if cwd != "" {
		fp := filepath.Join(cwd, p)
		if _, err := os.Stat(fp); err == nil {
			return filepath.Clean(fp), true
		}
	}
	return "", false
}

func (e *Engine) Compact(ctx context.Context) {
	log.Debugf("agent compacting: %d tokens, %d msgs", e.totalTokens, len(e.messages))
	if len(e.messages) < 12 {
		return
	}

	// Strategy: keep the first user message + last 6 messages intact.
	// Summarize everything in between.
	// Ensure we don't split assistant+tool pairs.
	keepTail := 6
	if keepTail > len(e.messages)-2 {
		keepTail = len(e.messages) - 2
	}

	// Find a safe split point — never split inside an assistant→tool pair
	splitIdx := len(e.messages) - keepTail
	for splitIdx > 1 && splitIdx < len(e.messages) {
		if e.messages[splitIdx].Role == "tool" {
			splitIdx-- // include the preceding assistant msg
		} else {
			break
		}
	}
	if splitIdx <= 1 {
		return // nothing worth compacting
	}

	history := e.messages[1:splitIdx]
	if len(history) < 4 {
		return
	}

	// Build a more structured summary request
	var summaryInput strings.Builder
	summaryInput.WriteString("Summarize this conversation history concisely. Structure:\n")
	summaryInput.WriteString("- Key decisions made\n- Files created/modified (paths)\n- Current task status\n- Errors encountered and resolutions\n- Important context for continuing\n\n")

	for _, m := range history {
		summaryInput.WriteString(fmt.Sprintf("[%s] ", m.Role))
		content := m.Content
		// For tool results, keep file paths but truncate output
		if m.Role == "tool" {
			if len(content) > 150 {
				content = content[:150] + "..."
			}
		} else if len(content) > 400 {
			content = content[:400] + "..."
		}
		summaryInput.WriteString(content)
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if path, ok := tc.Input["filePath"].(string); ok {
					summaryInput.WriteString(fmt.Sprintf(" → %s(%s)", tc.Name, path))
				} else if cmd, ok := tc.Input["command"].(string); ok {
					if len(cmd) > 60 {
						cmd = cmd[:60] + "..."
					}
					summaryInput.WriteString(fmt.Sprintf(" → bash(%s)", cmd))
				} else {
					summaryInput.WriteString(fmt.Sprintf(" → %s()", tc.Name))
				}
			}
		}
		summaryInput.WriteString("\n")
	}

	compactResp, err := e.provider.Chat(ctx, api.ChatRequest{
		Model:      e.config.Model,
		SystemBase: "You are a conversation summarizer. Be concise and factual. Output a structured summary under 400 words. Include file paths mentioned.",
		Messages:   []api.Message{{Role: "user", Content: summaryInput.String()}},
		MaxTokens:  800,
	})
	if err != nil {
		log.Debugf("compact failed: %v", err)
		return
	}

	// Rebuild message list: [first_user] + [summary] + [recent_tail]
	newMsgs := make([]api.Message, 0, 2+keepTail)
	newMsgs = append(newMsgs, e.messages[0]) // first user message
	newMsgs = append(newMsgs, api.Message{
		Role:    "user",
		Content: "[Context Summary]\n" + compactResp.Content + "\n\n[Continue the task from where you left off.]",
	})
	newMsgs = append(newMsgs, e.messages[splitIdx:]...) // recent messages preserved intact

	oldTokens := e.totalTokens
	oldMsgs := len(e.messages)
	e.messages = newMsgs
	e.totalTokens = countTokens(e.messages)
	log.Debugf("agent compacted: %d tokens/%d msgs -> %d tokens/%d msgs (kept tail %d)",
		oldTokens, oldMsgs, e.totalTokens, len(e.messages), keepTail)
}

func (e *Engine) buildAPIToolDefs() []api.ToolDef {
	if e.cachedToolDefs != nil {
		return e.cachedToolDefs
	}
	var defs []api.ToolDef
	for _, t := range e.registry.All() {
		d := t.Def()
		schema := parseSchema(d.InputSchema)
		defs = append(defs, api.ToolDef{
			Name: d.Name, Description: d.Description, InputSchema: schema,
		})
	}
	e.cachedToolDefs = defs
	return defs
}

func (e *Engine) LoadMessages(msgs []api.Message) {
	e.messages = msgs
	e.totalTokens = countTokens(msgs)
}

func (e *Engine) Messages() []api.Message { return e.messages }

func (e *Engine) saveSession() {
	if e.store == nil || e.session == nil {
		return
	}
	// Debounce: save at most every 5 seconds during rapid iterations
	now := time.Now()
	if !e.lastSaveTime.IsZero() && now.Sub(e.lastSaveTime) < 5*time.Second {
		return
	}
	e.lastSaveTime = now
	e.session.Messages = e.messages
	e.session.TokensIn = e.costTracker.TotalInput
	e.session.TokensOut = e.costTracker.TotalOutput
	e.session.Cost = e.costTracker.TotalCost
	e.session.UpdatedAt = now
	// Auto-set title from first user message if still default
	if e.session.Title == "New session" && len(e.messages) > 0 {
		for _, m := range e.messages {
			if m.Role == "user" && m.Content != "" {
				title := m.Content
				if len(title) > 60 {
					title = title[:60] + "..."
				}
				e.session.Title = title
				break
			}
		}
	}
	e.store.Save(e.session)
}

// SaveSession exports session persistence for the REPL to call on exit.
func (e *Engine) SaveSession() {
	e.lastSaveTime = time.Time{} // force save
	e.saveSession()
}

// HasMessages returns true if there are conversation messages worth saving.
func (e *Engine) HasMessages() bool { return len(e.messages) > 0 }

func countTokens(msgs []api.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)/4 + 1 // fast approximation: ~4 chars per token
		for _, tc := range m.ToolCalls {
			n += len(tc.Name)/4 + 1
			// Estimate input size without re-marshaling
			for k, v := range tc.Input {
				n += len(k)/4 + 1
				switch val := v.(type) {
				case string:
					n += len(val) / 4
				default:
					n += 10 // rough estimate for non-string values
				}
			}
		}
	}
	return n
}

func parseSchema(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return m
}

func summarizeResult(result string) string {
	s := strings.TrimSpace(result)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
func truncateTitle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func formatMessages(msgs []api.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Role + ": ")
		content := m.Content
		if len(content) > 300 {
			content = content[:297] + "..."
		}
		sb.WriteString(content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ANSI formatting for tool output lines
func formatToolLine(name, summary string, isError bool) string {
	const (
		reset = "\x1b[0m"
		dim   = "\x1b[2m"
		cyan  = "\x1b[36m"
		red   = "\x1b[31m"
		green = "\x1b[32m"
	)
	if isError {
		return fmt.Sprintf("  %s✗%s %s[%s]%s %s%s%s", red, reset, red, name, reset, red, summary, reset)
	}
	return fmt.Sprintf("  %s✓%s %s[%s]%s %s%s%s", green, reset, cyan, name, reset, dim, summary, reset)
}
