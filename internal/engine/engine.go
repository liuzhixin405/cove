package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/checkpoint"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/cost"
	"github.com/liuzhixin405/cove/internal/diagnostic"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/extract"
	"github.com/liuzhixin405/cove/internal/guardrail"
	"github.com/liuzhixin405/cove/internal/hooks"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/notes"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/session"
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/token"
	"github.com/liuzhixin405/cove/internal/tool"
)

const MaxIterations = 200
const CompactTokenThreshold = 64000

// maxParallelTools caps how many concurrency-safe tool calls run simultaneously
// within a single model response, preventing unbounded goroutine creation.
const maxParallelTools = 8

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
	provider              api.Provider
	registry              *tool.Registry
	messages              []api.Message
	config                Config
	projCtx               *ctxt.ProjectContext
	costTracker           *cost.Tracker
	perm                  *permission.Manager
	store                 *session.Store
	session               *session.Record
	memStore              *memory.Store
	skillMgr              *skills.Manager
	hookMgr               *hooks.Manager
	classifier            *permission.Classifier
	systemPrompt          string
	systemOverride        string
	totalTokens           int
	runtime               *tool.Runtime
	fileHistory           map[string]bool
	fileMu                sync.Mutex
	cachedToolDefs        []api.ToolDef
	cachedToolDefsVersion int
	lastSaveTime          time.Time
	consecutiveErrors     int        // track consecutive tool failures for circuit breaking
	iterCount             int        // track how many tool/LLM loops have run
	promptMu              sync.Mutex // lock for interactive permission prompts
	PermissionPrompt      func(toolName string, input map[string]any, reason string) bool
	OnPermissionPause     func()                       // called before permission prompt to pause spinners
	OnPermissionDone      func()                       // called after permission decision to resume
	OnToolProgress        func(toolName, chunk string) // live output chunks from long-running tools
	sessionNotes          *notes.SessionNotes
	guardrails            *guardrail.Tracker
	subdirHints           *ctxt.SubdirHints
	rateLimits            *api.RateLimitTracker
	extractRunner         *extract.Runner
	dreamRunner           *dream.Runner
	cpMgr                 *checkpoint.Manager
	lastReviewMsgCount    int

	// Activity tracking powers the stall monitor: every blocking stage (model
	// call, tool execution, compaction) registers an activity so that, when the
	// app appears to hang ("一直无响应"), we can name exactly which stage is stuck.
	actMu  sync.Mutex
	acts   map[uint64]*activity
	actSeq uint64
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
	store, err := session.NewStore()
	if err != nil {
		return nil, fmt.Errorf("failed to init session store: %w", err)
	}

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

	// Initialize session notes
	cwd, _ := os.Getwd()
	if cwd != "" {
		e.sessionNotes = notes.New(cwd)
		e.sessionNotes.Load()
	} else {
		e.sessionNotes = notes.NewGlobal()
	}

	// Initialize guardrails (tool loop detection)
	e.guardrails = guardrail.New()

	// Initialize subdirectory hints tracker
	if cwd != "" {
		e.subdirHints = ctxt.NewSubdirHints(cwd)
	}

	// Initialize rate limit tracker
	e.rateLimits = api.NewRateLimitTracker()

	// Initialize extract runner (auto memory extraction)
	e.extractRunner = extract.NewRunner(prov, config.Model)

	// Initialize dream runner (periodic memory consolidation)
	e.dreamRunner = dream.NewRunner(prov, config.Model, e.session.ID)

	// Initialize checkpoint manager (git-based file snapshots)
	if cpMgr, err := checkpoint.New(cwd); err == nil {
		e.cpMgr = cpMgr
	} else {
		log.Debugf("[checkpoint] init failed: %v", err)
	}

	// Fire session start hooks
	if e.hookMgr != nil {
		e.hookMgr.Fire(context.Background(), hooks.SessionStart, nil)
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
func (e *Engine) Provider() api.Provider     { return e.provider }
func (e *Engine) SetPermissionMode(mode permission.Mode) {
	if permission.ValidMode(mode) {
		e.perm.SetMode(mode)
		e.config.PermissionMode = string(mode)
	}
}

func (e *Engine) SetMaxBudget(maxBudget float64) {
	e.config.MaxBudget = maxBudget
	if e.costTracker != nil {
		e.costTracker.MaxBudget = maxBudget
	}
}

func (e *Engine) AddPermissionRule(decision permission.Decision, rule permission.Rule) {
	e.perm.AddRule(decision, rule)
}

func (e *Engine) Registry() *tool.Registry          { return e.registry }
func (e *Engine) Runtime() *tool.Runtime            { return e.runtime }
func (e *Engine) RateLimits() *api.RateLimitTracker { return e.rateLimits }
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

	// Inject session notes for context continuity
	if e.sessionNotes != nil {
		if nc := e.sessionNotes.Content(); nc != "" {
			sb.WriteString(nc)
		}
	}

	if e.projCtx != nil {
		sb.WriteString(fmt.Sprintf("\n\nWorking directory: %s | Platform: %s | Shell: %s",
			e.projCtx.Cwd, e.projCtx.Platform, e.projCtx.Shell))
		if e.projCtx.IsGitRepo {
			sb.WriteString(fmt.Sprintf("\nGit: %s (%s)", e.projCtx.GitBranch, e.projCtx.GitStatus))
			if e.projCtx.GitMain != "" && e.projCtx.GitMain != e.projCtx.GitBranch {
				sb.WriteString(fmt.Sprintf(" | main branch: %s", e.projCtx.GitMain))
			}
			if e.projCtx.GitUser != "" {
				sb.WriteString(fmt.Sprintf(" | user: %s", e.projCtx.GitUser))
			}
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

func (e *Engine) IterCount() int { return e.iterCount }
func (e *Engine) Run(ctx context.Context, userMessage string) (string, error) {
	return e.RunWithStream(ctx, userMessage, nil)
}

func (e *Engine) RunWithStream(ctx context.Context, userMessage string, onDelta func(delta string)) (string, error) {
	return e.RunMessageWithStream(ctx, api.Message{Role: "user", Content: userMessage}, onDelta, nil)
}

func (e *Engine) RunMessageWithStream(ctx context.Context, userMessage api.Message, onDelta func(delta string), onReasoning func(reasoning string)) (string, error) {
	if e.costTracker.OverBudget() {
		return "", fmt.Errorf("budget exceeded: %s", e.costTracker.Summary())
	}

	// Stall monitor: surfaces which stage is stuck if the run appears to hang.
	stopMonitor := make(chan struct{})
	go e.runStallMonitor(stopMonitor)
	defer close(stopMonitor)

	if userMessage.Role == "" {
		userMessage.Role = "user"
	}
	prevMessages := append([]api.Message(nil), e.messages...)
	e.messages = append(e.messages, userMessage)
	e.saveSession()

	// Cache system prompt and tool defs across iterations (stable within a run)
	sp := e.SystemPrompt()
	toolDefs := e.buildAPIToolDefs()

	for iter := 0; iter < MaxIterations; iter++ {
		e.iterCount = iter + 1
		// Bail out immediately if the context has been cancelled (e.g. user pressed Ctrl+C)
		if ctx.Err() != nil {
			e.messages = prevMessages
			e.saveSession()
			return "", ctx.Err()
		}
		log.Debugf("agent iter=%d msgs=%d tokens=%d tools=%d model=%s cost=%s",
			iter, len(e.messages), e.totalTokens, len(toolDefs), e.config.Model, e.costTracker.Summary())

		// Apply prompt cache breakpoints for Anthropic
		reqMessages := e.messages
		if e.provider.Name() == "anthropic" {
			reqMessages = api.InjectCacheBreakpoints(e.messages)
		}

		req := api.ChatRequest{
			Model:      e.config.Model,
			Messages:   reqMessages,
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
			walker = repl.NewWalkingIndicator("思考中...")
			walker.Start()
		}

		if useStream {
			firstDelta := true
			modelAct := e.beginActivity("调用模型 " + e.config.Model)
			resp, err = e.provider.ChatStream(ctx, req, func(ev api.StreamEvent) {
				e.progressActivity(modelAct)
				if firstDelta && walker != nil {
					walker.Stop()
					walker = nil
					firstDelta = false
				}
				if ev.Type == "delta" && onDelta != nil {
					onDelta(ev.Delta)
				}
				if ev.Type == "reasoning" && onReasoning != nil {
					onReasoning(ev.Reasoning)
				}
			})
			e.endActivity(modelAct)
		} else {
			modelAct := e.beginActivity("调用模型 " + e.config.Model)
			resp, err = e.provider.Chat(ctx, req)
			e.endActivity(modelAct)
		}

		if walker != nil {
			walker.Stop()
		}

		if err != nil {
			e.messages = prevMessages
			e.saveSession()
			diagnostic.RecordRuntime(diagnostic.SevError, diagnostic.CatAPI,
				fmt.Sprintf("模型调用失败: %s", err.Error()))
			return "", fmt.Errorf("api: %w", err)
		}

		e.costTracker.AddDetailed(e.config.Model, resp.InputTokens, resp.OutputTokens, resp.PromptCacheHitTokens, resp.PromptCacheMissTokens)

		// Update rate limit tracking
		if e.rateLimits != nil && resp.RateLimitHeaders != nil {
			e.rateLimits.Update(resp.RateLimitHeaders)
		}

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
			// Turn-end pipeline (all run in background)
			e.runTurnEndPipeline()
			// Auto-track decisions and discoveries
			e.recordSignals(userMessage.Content, resp.Content)
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
			// Partition tool calls into concurrent-safe and serial groups.
			// Additionally, write/edit calls targeting different files can run in parallel.
			var wg sync.WaitGroup
			serialFilePaths := make(map[string]bool) // track files being written to serialize conflicts
			// Bound concurrency so a single response with many tool calls cannot
			// spawn an unbounded number of goroutines.
			sem := make(chan struct{}, maxParallelTools)

			for i, tc := range resp.ToolCalls {
				t, _ := e.registry.Find(tc.Name)
				safe := t != nil && t.Def().IsConcurrencySafe

				// write/edit to distinct files can also be parallelized
				if !safe && (tc.Name == "write" || tc.Name == "edit") {
					if fp, ok := tc.Input["filePath"].(string); ok && fp != "" {
						if !serialFilePaths[fp] {
							serialFilePaths[fp] = true
							safe = true // different file, safe to parallelize
						}
					}
				}

				if safe {
					wg.Add(1)
					sem <- struct{}{}
					go func(idx int, tcall api.ToolCall) {
						defer wg.Done()
						defer func() { <-sem }()
						defer func() {
							if r := recover(); r != nil {
								results[idx] = toolResult{ID: tcall.ID, Name: tcall.Name, Content: fmt.Sprintf("Error: tool panicked: %v", r)}
							}
						}()
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
			isErr := strings.HasPrefix(r.Content, "Error:")
			if !e.config.Debug {
				fmt.Fprintf(os.Stderr, "\r\x1b[K%s\n", formatToolLine(r.Name, summarizeResult(r.Content), isErr))
			}
			// Session notes capture (always, regardless of debug mode)
			if e.sessionNotes != nil {
				if isErr {
					e.sessionNotes.AddError(fmt.Sprintf("%s: %s", r.Name, summarizeResult(r.Content)))
				}
			}
			if isErr {
				diagnostic.RecordRuntime(diagnostic.SevWarning, diagnostic.CatTool,
					fmt.Sprintf("工具 %s 失败: %s", r.Name, summarizeResult(r.Content)))
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
		if e.totalTokens > CompactTokenThreshold && iter > 5 && len(e.messages) > 16 {
			compactAct := e.beginActivity("压缩上下文")
			e.Compact(ctx)
			e.endActivity(compactAct)
		}
	}

	return "", fmt.Errorf("max iterations (%d) reached, cost: %s", MaxIterations, e.costTracker.Summary())
}

func (e *Engine) executeTool(ctx context.Context, tc api.ToolCall) string {
	t, ok := e.registry.Find(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}

	// Track this tool as an in-flight stage so a hung tool (e.g. a bash command
	// or MCP call that ignores ctx) is attributable by the stall monitor.
	toolAct := e.beginActivity("执行工具 " + tc.Name)
	defer e.endActivity(toolAct)

	// Fire pre-tool-use hooks
	if e.hookMgr != nil {
		e.hookMgr.Fire(ctx, hooks.PreToolUse, hooks.ToolUseInfo{
			ToolName: tc.Name,
			Input:    tc.Input,
		})
	}
	// Guardrail check before execution
	if e.guardrails != nil {
		decision := e.guardrails.BeforeCall(tc.Name, tc.Input)
		switch decision.Action {
		case guardrail.Block:
			return fmt.Sprintf("Error: %s", decision.Message)
		case guardrail.Warn:
			// Inject warning but proceed
			log.Debugf("guardrail warn: %s %s", tc.Name, decision.Message)
		}
	}

	cwd := ""
	if e.projCtx != nil {
		cwd = e.projCtx.Cwd
	}
	// Auto-checkpoint before file-mutating operations
	if e.cpMgr != nil && (tc.Name == "write" || tc.Name == "edit") {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warnf("[checkpoint] panic: %v", r)
				}
			}()
			if hash, err := e.cpMgr.Create("auto-" + tc.Name); err == nil && hash != "" {
				log.Debugf("[checkpoint] %s", hash[:8])
			}
		}()
	}
	tctx := tool.Context{
		Cwd:              cwd,
		ToolUseID:        tc.ID,
		PermissionMode:   toolPermissionMode(e.perm.Mode()),
		IsNonInteractive: e.runtime == nil || e.runtime.AskUser == nil,
		Debug:            e.config.Debug,
		Runtime:          e.runtime,
		// Forward live tool output: reset the stall timer so an actively
		// producing command isn't mislabeled as "stuck", and surface the
		// chunk to the UI so the user can see what the command is doing.
		OnProgress: func(chunk string) {
			e.progressActivity(toolAct)
			if e.OnToolProgress != nil {
				e.OnToolProgress(tc.Name, chunk)
			}
		},
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
				if e.OnPermissionPause != nil {
					e.OnPermissionPause()
				}
				e.pauseActivity(toolAct, true) // waiting on user, not stuck
				e.promptMu.Lock()
				approved := e.PermissionPrompt(tc.Name, tc.Input, reason)
				e.promptMu.Unlock()
				e.pauseActivity(toolAct, false)
				if e.OnPermissionDone != nil {
					e.OnPermissionDone()
				}
				if !approved {
					return fmt.Sprintf("Error: permission denied for %s: user rejected", tc.Name)
				}
			} else {
				return fmt.Sprintf("Error: permission denied for %s: user rejected", tc.Name)
			}
		} else {
			return fmt.Sprintf("Error: permission denied for %s: %s", tc.Name, reason)
		}
	}

	result, err := t.Call(ctx, tc.Input, tctx)
	if err != nil {
		// Retry once for transient errors (network, timeout, temporary file locks)
		if isTransientError(err) {
			time.Sleep(100 * time.Millisecond)
			result, err = t.Call(ctx, tc.Input, tctx)
			if err != nil {
				if e.guardrails != nil {
					e.guardrails.AfterCall(tc.Name, tc.Input, err.Error(), true)
				}
				return fmt.Sprintf("Error (after retry): %v", err)
			}
		} else {
			if e.guardrails != nil {
				e.guardrails.AfterCall(tc.Name, tc.Input, err.Error(), true)
			}
			return fmt.Sprintf("Error: %v", err)
		}
	}
	if !result.IsError {
		e.trackFileChanges(tc)
	}
	output := result.Data

	// Record result in guardrails
	if e.guardrails != nil {
		e.guardrails.AfterCall(tc.Name, tc.Input, output, result.IsError)
	}
	// Fire post-tool-use hooks
	if e.hookMgr != nil {
		e.hookMgr.Fire(ctx, hooks.PostToolUse, hooks.ToolUseInfo{
			ToolName: tc.Name,
			Input:    tc.Input,
			Result:   output,
			IsError:  result.IsError,
		})
	}

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

		// Conditional skills: inject matching skill prompts based on file type
		if e.skillMgr != nil {
			if filePath, ok := tc.Input["filePath"].(string); ok && filePath != "" {
				if prompt := e.skillMgr.MatchingPrompt(filePath); prompt != "" {
					output += prompt
				}
			}
		}
		// Subdirectory hints: inject context from discovered AGENTS.md files
		if e.subdirHints != nil {
			var hint string
			if path, ok := tc.Input["filePath"].(string); ok {
				hint = e.subdirHints.CheckPath(path)
			} else if cmd, ok := tc.Input["command"].(string); ok {
				hint = e.subdirHints.CheckCommand(cmd)
			}
			if hint != "" {
				output += hint
			}
		}
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

func shortPath(input map[string]any) string {
	if p, ok := input["filePath"].(string); ok {
		return filepath.Base(p)
	}
	if cmd, ok := input["command"].(string); ok {
		if len(cmd) > 40 {
			return cmd[:40] + "..."
		}
		return cmd
	}
	return ""
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
			if e.sessionNotes != nil {
				e.sessionNotes.AddTask(fmt.Sprintf("File: %s", filepath.Base(path)))
			}
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
	if e.sessionNotes != nil {
		e.sessionNotes.AddDecision(fmt.Sprintf("Context compacted at %d tokens, %d messages", e.totalTokens, len(e.messages)))
	}
	if len(e.messages) < 12 {
		return
	}

	// Layer 1: Trim old tool results to 1-line summaries (cheap, no API call)
	e.trimOldToolResults()

	// Recount after trimming
	e.totalTokens = countTokens(e.messages)
	if e.totalTokens <= CompactTokenThreshold {
		log.Debugf("compact: trimming alone sufficient (%d tokens)", e.totalTokens)
		return
	}

	// Layer 2: LLM summary of middle conversation
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
		// For tool results, keep file paths but truncate output aggressively
		if m.Role == "tool" {
			if len(content) > 100 {
				content = content[:100] + "..."
			}
		} else if len(content) > 250 {
			content = content[:250] + "..."
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
		SystemBase: "You are a conversation summarizer. Be concise and factual. Output a structured summary under 300 words. Include file paths mentioned.",
		Messages:   []api.Message{{Role: "user", Content: summaryInput.String()}},
		MaxTokens:  600,
	})
	if err != nil {
		log.Warnf("compact failed: %v", err)
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

// trimOldToolResults replaces verbose tool results in old messages with 1-line summaries.
// Preserves the last 6 messages intact. This is a cheap pre-processing step.
func (e *Engine) trimOldToolResults() {
	if len(e.messages) <= 8 {
		return
	}
	// Only trim messages before the last 6
	cutoff := len(e.messages) - 6
	for i := 0; i < cutoff; i++ {
		m := &e.messages[i]
		if m.Role != "tool" {
			continue
		}
		if len(m.Content) <= 200 {
			continue // already short
		}
		// Generate a 1-line summary: [tool_name] first_line... (N chars)
		firstLine := m.Content
		if idx := strings.IndexByte(firstLine, '\n'); idx > 0 {
			firstLine = firstLine[:idx]
		}
		if len(firstLine) > 80 {
			firstLine = firstLine[:80] + "..."
		}
		m.Content = fmt.Sprintf("[%s] %s (%d chars原始输出已压缩)", m.Name, firstLine, len(m.Content))
	}
}

func (e *Engine) buildAPIToolDefs() []api.ToolDef {
	if e.cachedToolDefs != nil && e.cachedToolDefsVersion == e.registry.Version() {
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
	e.cachedToolDefsVersion = e.registry.Version()
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
	// Debounce: save at most every 10 seconds during rapid iterations
	now := time.Now()
	if !e.lastSaveTime.IsZero() && now.Sub(e.lastSaveTime) < 10*time.Second {
		return
	}
	e.lastSaveTime = now
	e.session.Messages = e.messages
	e.session.TokensIn = e.costTracker.TotalInput
	e.session.TokensOut = e.costTracker.TotalOutput
	e.session.Cost = e.costTracker.TotalCost
	e.session.UpdatedAt = now
	// Auto-set title from user intent and repair low-signal legacy titles.
	if len(e.messages) > 0 && (e.session.Title == "New session" || e.session.Title == "" || isLowSignalSessionTitle(e.session.Title)) {
		if title := pickSessionTitle(e.messages); title != "" {
			e.session.Title = title
		}
	}
	e.store.Save(e.session)
}

func pickSessionTitle(messages []api.Message) string {
	fallback := ""
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		if len(text) > 60 {
			text = text[:60] + "..."
		}
		if !isLowSignalSessionTitle(text) {
			return text
		}
		if fallback == "" {
			fallback = text
		}
	}
	return fallback
}

func isLowSignalSessionTitle(s string) bool {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return true
	}
	if len([]rune(v)) <= 2 {
		return true
	}
	noise := map[string]bool{
		"write":        true,
		"write a file": true,
		"read":         true,
		"read file":    true,
		"grep":         true,
		"continue":     true,
		"继续":           true,
		"hi":           true,
		"hello":        true,
		"你好":           true,
		"?":            true,
	}
	if noise[v] {
		return true
	}
	if strings.HasPrefix(v, "/") {
		return true
	}
	return false
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
	for i := range msgs {
		n += len(msgs[i].Content)/4 + 1 // fast approximation: ~4 chars per token
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			n += len(tc.Name)/4 + 1
			for k, v := range tc.Input {
				n += len(k)/4 + 1
				if val, ok := v.(string); ok {
					n += len(val) / 4
				} else {
					n += 10
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
	if len(s) <= 80 {
		return s
	}

	// Preserve full file paths for common tool summaries like:
	// "Wrote 123 bytes to D:\\path\\file.txt" or "Read ... from /tmp/a.txt"
	if kept, ok := preservePathSummary(s, " to "); ok {
		return kept
	}
	if kept, ok := preservePathSummary(s, " from "); ok {
		return kept
	}
	if kept, ok := preservePathSummary(s, "File: "); ok {
		return kept
	}
	if kept, ok := preservePathSummary(s, "file not found: "); ok {
		return kept
	}
	if kept, ok := preservePathSummary(s, "Path: "); ok {
		return kept
	}

	if kept, ok := preservePathTokenLine(s); ok {
		return kept
	}

	return s[:77] + "..."
}

func preservePathSummary(s, marker string) (string, bool) {
	idx := strings.LastIndex(s, marker)
	if idx < 0 {
		return "", false
	}
	pathPart := strings.TrimSpace(s[idx+len(marker):])
	if pathPart == "" {
		return "", false
	}
	if !looksLikePath(pathPart) {
		return "", false
	}

	head := s[:idx+len(marker)]
	if len(head) > 40 {
		head = head[:37] + "..."
	}
	return head + pathPart, true
}

func preservePathTokenLine(s string) (string, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", false
	}

	best := ""
	for _, f := range fields {
		candidate := strings.Trim(f, "\"'()[]{}<>,;")
		if looksLikePath(candidate) && len(candidate) > len(best) {
			best = candidate
		}
	}
	if best == "" {
		return "", false
	}

	idx := strings.Index(s, best)
	if idx < 0 {
		return best, true
	}

	prefix := strings.TrimSpace(s[:idx])
	suffix := strings.TrimSpace(s[idx+len(best):])

	if len(prefix) > 40 {
		prefix = prefix[:37] + "..."
	}
	if len(suffix) > 24 {
		suffix = suffix[:21] + "..."
	}

	if prefix == "" && suffix == "" {
		return best, true
	}
	if suffix == "" {
		if prefix == "" {
			return best, true
		}
		return prefix + " " + best, true
	}
	if prefix == "" {
		return best + " " + suffix, true
	}
	return prefix + " " + best + " " + suffix, true
}

func looksLikePath(s string) bool {
	if s == "" {
		return false
	}
	if !strings.Contains(s, "\\") && !strings.Contains(s, "/") {
		return false
	}
	if strings.Contains(s, ":\\") || strings.Contains(s, ":/") {
		return true
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "~") {
		return true
	}
	// Accept nested relative paths like foo/bar/baz.cs
	if strings.Count(s, "/")+strings.Count(s, "\\") >= 2 {
		return true
	}
	return false
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

// runTurnEndPipeline executes quiet post-turn persistence only.
func (e *Engine) runTurnEndPipeline() {
	// Flush session notes (sync, fast I/O)
	if e.sessionNotes != nil {
		e.sessionNotes.Flush()
	}
	// Extract durable memories from recent conversation (async, throttled internally)
	if e.extractRunner != nil && len(e.messages) > 0 {
		// Snapshot the slice so the background goroutine never reads e.messages
		// while a subsequent turn appends to it (data race).
		msgs := append([]api.Message(nil), e.messages...)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warnf("[extractMemories] panic: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			e.extractRunner.Extract(ctx, msgs)
		}()
	}
	// Background review: auto-create skills/memories from conversation patterns
	e.backgroundReview()
	// Fire auto-dream consolidation if conditions met (async, throttled internally)
	if e.dreamRunner != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warnf("[autoDream] panic: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			e.dreamRunner.ExecuteAutoDream(ctx)
		}()
	}
}

// decision/discovery patterns for auto-tracking in session notes
var (
	decisionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:use|using|we.ll use|go with|let.s use|switch to|prefer|stick with)\s+(.+?)(?:\.|$)`),
		regexp.MustCompile(`(?i)(?:I prefer|I like|I want|let.s go with)\s+(.+?)(?:\.|$)`),
	}
	discoveryPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:I found|discovered|the issue is|the reason is|it turns out)\s+(.+?)(?:\.|$)`),
		regexp.MustCompile(`(?i)(?:fixed by|resolved by|solved by)\s+(.+?)(?:\.|$)`),
	}
)

// recordSignals scans user/assistant messages for decisions and discoveries, saving to session notes.
func (e *Engine) recordSignals(userMsg, assistantMsg string) {
	if e.sessionNotes == nil {
		return
	}
	for _, p := range decisionPatterns {
		if m := p.FindStringSubmatch(userMsg); len(m) > 1 {
			text := strings.TrimSpace(m[1])
			if len(text) > 3 && len(text) < 200 {
				e.sessionNotes.AddDecision(text)
			}
		}
	}
	for _, p := range discoveryPatterns {
		if m := p.FindStringSubmatch(assistantMsg); len(m) > 1 {
			text := strings.TrimSpace(m[1])
			if len(text) > 3 && len(text) < 200 {
				e.sessionNotes.AddDiscovery(text)
			}
		}
	}
}

// SetAutoExtract enables/disables automatic background memory extraction.
func (e *Engine) SetAutoExtract(on bool) {
	// Background extraction and review are always enabled now.
	// This method is kept for CLI compatibility.
}

// SessionNotes returns the session notes manager.
func (e *Engine) SessionNotes() *notes.SessionNotes {
	return e.sessionNotes
}
