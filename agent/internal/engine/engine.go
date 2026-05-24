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
	"github.com/agentgo/internal/session"
	"github.com/agentgo/internal/skills"
	"github.com/agentgo/internal/token"
	"github.com/agentgo/internal/tool"
)

const MaxIterations = 80
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
	provider       api.Provider
	registry       *tool.Registry
	messages       []api.Message
	config         Config
	projCtx        *ctxt.ProjectContext
	costTracker    *cost.Tracker
	perm           *permission.Manager
	store          *session.Store
	session        *session.Record
	memStore       *memory.Store
	skillMgr       *skills.Manager
	hookMgr        *hooks.Manager
	classifier     *permission.Classifier
	systemPrompt   string
	systemOverride string
	totalTokens    int
	runtime        *tool.Runtime
	fileHistory    map[string]bool
	fileMu         sync.Mutex
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
			Tasks:        make(map[string]*tool.TaskRecord),
			SkillManager: config.SkillManager,
			SkillPrompts: make(map[string]string),
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
	var sb strings.Builder
	sb.WriteString(`You are an AI coding assistant. You MUST use tools to complete user tasks. Never describe what you would do — actually DO it.

RULES:
1. Use tools for ALL file ops, command execution, code search, web access.
2. Single-step tasks: use the tool immediately, no explanation needed.
3. Multi-step tasks: use todowrite to track progress.
4. Be concise. Use tools to act, not to describe actions.
5. For git, tests, builds — use bash. For files — write/read/edit.
6. Use webfetch for URLs. Use grep/glob for searching code.

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
			MaxTokens:  16000,
		}

		var resp *api.ChatResponse
		var err error
		useStream := onDelta != nil

		if useStream {
			resp, err = e.provider.ChatStream(ctx, req, func(ev api.StreamEvent) {
				if ev.Type == "delta" && onDelta != nil {
					onDelta(ev.Delta)
				}
			})
		} else {
			resp, err = e.provider.Chat(ctx, req)
		}

		if err != nil {
			return "", fmt.Errorf("api: %w", err)
		}

		e.costTracker.AddDetailed(e.config.Model, resp.InputTokens, resp.OutputTokens, resp.PromptCacheHitTokens, resp.PromptCacheMissTokens)

		log.Debugf("agent text=%d tools=%d in=%d out=%d stop=%s",
			len(resp.Content), len(resp.ToolCalls), resp.InputTokens, resp.OutputTokens, resp.StopReason)

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
				res := e.executeTool(ctx, tc)
				results[i] = toolResult{ID: tc.ID, Name: tc.Name, Content: res}
			}
		}

		for _, r := range results {
			if !e.config.Debug {
				fmt.Fprintf(os.Stderr, "\r  [%s] %s\n", r.Name, summarizeResult(r.Content))
			}
			e.messages = append(e.messages, api.Message{
				Role: "tool", ToolCallID: r.ID, Name: r.Name, Content: r.Content,
			})
		}

		e.totalTokens = countTokens(e.messages)
		if e.totalTokens > CompactTokenThreshold && iter > 5 {
			e.compact(ctx)
		}
	}

	return "", fmt.Errorf("max iterations (%d) reached, cost: %s", MaxIterations, e.costTracker.Summary())
}

func (e *Engine) executeTool(ctx context.Context, tc api.ToolCall) string {
	t, ok := e.registry.Find(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}

	tctx := tool.Context{
		Cwd:              e.projCtx.Cwd,
		ToolUseID:        tc.ID,
		PermissionMode:   string(e.perm.Mode()),
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

	result, err := t.Call(ctx, tc.Input, tctx)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if !result.IsError {
		e.trackFileChanges(tc)
	}
	output := result.Data
	if !result.IsError {
		output = token.TruncateToTokens(output, 4000)
	}
	if result.IsError {
		return fmt.Sprintf("Error: %s", result.Data)
	}
	return output
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

func (e *Engine) compact(ctx context.Context) {
	log.Debugf("agent compacting: %d tokens, %d msgs", e.totalTokens, len(e.messages))
	if len(e.messages) < 10 {
		return
	}
	history := e.messages[1 : len(e.messages)-3]
	if len(history) < 6 {
		return
	}

	compactReq := fmt.Sprintf(
		"Summarize this conversation segment concisely. Include: key decisions, files created/modified, errors encountered, current state. Keep it under 500 words.\n\n%s",
		formatMessages(history))

	compactResp, err := e.provider.Chat(ctx, api.ChatRequest{
		Model:      e.config.Model,
		SystemBase: "You are a conversation summarizer. Be concise and factual. Only include what's essential for continuing the work.",
		Messages:   []api.Message{{Role: "user", Content: compactReq}},
		MaxTokens:  1000,
	})
	if err != nil {
		return
	}

	newMsgs := make([]api.Message, 0, 3)
	newMsgs = append(newMsgs, e.messages[0])
	newMsgs = append(newMsgs, api.Message{
		Role:    "user",
		Content: "[COMPACTED] " + compactResp.Content + "\n\nContinue from where you left off.",
	})
	newMsgs = append(newMsgs, e.messages[len(e.messages)-1])
	e.messages = newMsgs
	oldTokens := e.totalTokens
	e.totalTokens = countTokens(e.messages)
	log.Debugf("agent compacted: %d tokens, %d msgs -> %d tokens, %d msgs",
		oldTokens, len(history), e.totalTokens, len(e.messages))
}

func (e *Engine) buildAPIToolDefs() []api.ToolDef {
	var defs []api.ToolDef
	for _, t := range e.registry.All() {
		d := t.Def()
		schema := parseSchema(d.InputSchema)
		defs = append(defs, api.ToolDef{
			Name: d.Name, Description: d.Description, InputSchema: schema,
		})
	}
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
	e.session.Messages = e.messages
	e.session.TokensIn = e.costTracker.TotalInput
	e.session.TokensOut = e.costTracker.TotalOutput
	e.session.Cost = e.costTracker.TotalCost
	e.session.UpdatedAt = time.Now()
	e.store.Save(e.session)
}

func countTokens(msgs []api.Message) int {
	n := 0
	for _, m := range msgs {
		n += token.Estimate(m.Content)
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Input)
			n += token.Estimate(tc.Name + string(args))
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
