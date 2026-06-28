package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/checkpoint"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/cost"
	"github.com/liuzhixin405/cove/internal/delegate"
	"github.com/liuzhixin405/cove/internal/diagnostic"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/extract"
	"github.com/liuzhixin405/cove/internal/guardrail"
	"github.com/liuzhixin405/cove/internal/hooks"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/notes"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/plan"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/repomap"
	"github.com/liuzhixin405/cove/internal/safety"
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
	ModelFast      string
	PermissionMode string
	MaxBudget      float64
	Debug          bool
	Tools          []tool.Tool
	Provider       api.ProviderConfig
	MemoryStore    *memory.Store
	SkillManager   *skills.Manager
	HookManager    *hooks.Manager
	Classifier     *permission.Classifier
	LoopDetectionDisabled bool
}

type Engine struct {
	fallback              *api.ModelFallback
	modelRouter           *api.ModelRouter
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
	steerMu               sync.Mutex
	pendingSteer          string
	cachedToolDefs        []api.ToolDef
	cachedToolDefsVersion int
	lastSaveTime          time.Time
	consecutiveErrors     int        // track consecutive tool failures for circuit breaking
	loopHistory          []string       // recent tool-call fingerprints for loop detection
	loopDetector         *LoopDetector // enhanced 2-layer loop detection (P0)
	compressor           *ChatCompressor // AI-powered conversation compression (P0-3)
	masker               *ToolOutputMasker // tool output masking to save context (P1)
	nextSpeaker          *NextSpeaker      // predicts when to yield to user (P1)
	safetyChecker        *safety.Checker   // security scan before tool execution (P1)
	policyEngine         *permission.PolicyEngine // rule-based permission policies (P2)
	sessionView          *session.SessionView     // snapshot for change tracking (P2)
	enhancedRepoMap      *repomap.EnhancedGenerator // incremental repo map (P2)
	iterCount             int        // track how many tool/LLM loops have run
	promptMu              sync.Mutex // lock for interactive permission prompts
	// OnEngineOutput, if set, receives engine diagnostic lines
	// (tool progress, spinner, etc.) instead of writing to stderr.
	OnEngineOutput     func(line string)
	PermissionPrompt   func(toolName string, input map[string]any, reason string) bool
	OnPermissionPause  func()                       // called before permission prompt to pause spinners
	OnPermissionDone   func()                       // called after permission decision to resume
	OnToolProgress     func(toolName, chunk string) // live output chunks from long-running tools
	sessionNotes       *notes.SessionNotes
	guardrails         *guardrail.Tracker
	subdirHints        *ctxt.SubdirHints
	rateLimits         *api.RateLimitTracker
	extractRunner      *extract.Runner
	dreamRunner        *dream.Runner
	cpMgr              *checkpoint.Manager
	lastReviewMsgCount int

	// Activity tracking powers the stall monitor: every blocking stage (model
	// call, tool execution, compaction) registers an activity so that, when the
	// app appears to hang ("һֱ����Ӧ"), we can name exactly which stage is stuck.
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

	// Create model router for dual-model switching
	modelRouter := api.NewModelRouter(config.Model, config.ModelFast)

	e := &Engine{
		fallback:    api.NewModelFallback([]api.Provider{prov}),
		modelRouter: modelRouter,
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

	if !config.LoopDetectionDisabled {
		// Use model-aware thresholds: fast (flash) models get more sensitive
		// because they're more prone to getting stuck in repetitive loops.
		// Detect fast models by checking if the configured model name contains
		// fast/flash/mini/lite indicators, rather than comparing ModelFast==Model
		// (which breaks when model and model_fast are different).
		isFast := isFastModelName(config.Model)
		e.loopDetector = NewLoopDetectorWithModel(isFast)
	}
	e.compressor = NewChatCompressor()
	e.masker = NewToolOutputMasker()
	e.nextSpeaker = NewNextSpeaker()
	e.safetyChecker = safety.New()
	e.policyEngine = permission.NewPolicyEngine()

	// Load permission policies from disk if available
	if home, err := os.UserHomeDir(); err == nil {
		policyStore, err := permission.NewFilePolicyStorage(filepath.Join(home, ".cove", "policies.json"))
		if err == nil {
			if rules, err := policyStore.Load(); err == nil && len(rules) > 0 {
				e.policyEngine.LoadRules(rules)
			}
		}
	}

	if config.SkillManager != nil {
		for _, s := range config.SkillManager.All() {
			e.runtime.SkillPrompts[s.Name] = s.Prompt
		}
	}

	// Initialize session view for change tracking
	e.sessionView = session.NewSessionView(e.messages, 0)

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
		e.enhancedRepoMap = repomap.NewEnhancedGenerator(cwd)
	} else {
		e.sessionNotes = notes.NewGlobal()
		e.enhancedRepoMap = repomap.NewEnhancedGenerator(".")
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
		e.hookMgr.FireLegacy(context.Background(), hooks.SessionStart, nil)
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
	if prov != nil {
		e.fallback = api.NewModelFallback([]api.Provider{prov})
	}
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
func (e *Engine) ProviderName() string       { return e.fallback.Current().DisplayName() }
func (e *Engine) Provider() api.Provider     { return e.fallback.Current() }
// SetProvider replaces the current provider chain with a single-provider fallback.
// Used primarily by tests to inject mock providers.
func (e *Engine) SetProvider(p api.Provider) { e.fallback = api.NewModelFallback([]api.Provider{p}) }
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
	sb.WriteString(`You are an AI coding assistant. You MUST use tools to complete user tasks. Never describe what you would do �� actually DO it.

RULES:
1. Use tools for ALL file ops, command execution, code search, web access.
2. Single-step tasks: use the tool immediately, no explanation needed.
3. Multi-step tasks: use todowrite to track progress.
4. Be concise. Use tools to act, not to describe actions.
5. For git, tests, builds �� use bash. For files �� write/read/edit.
6. Use webfetch for URLs. Use grep/glob for searching code.
7. For creating or fully rewriting files (especially large ones like HTML/CSS/JS): use write with the COMPLETE content in ONE call. Do NOT use many small edit calls for new files.
8. Each tool call response is ONE file operation. Do NOT attempt to write multiple large files in a single response �� write them one at a time across iterations.

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
		// Use enhanced incremental repo map when available
		if e.enhancedRepoMap != nil {
			if mapText, _ := e.enhancedRepoMap.GenerateIncremental(200); mapText != "" {
				sb.WriteString(fmt.Sprintf("\n<repo_map>\n%s\n</repo_map>\n", mapText))
			}
		} else if e.projCtx.RepoMap != "" {
			sb.WriteString(fmt.Sprintf("\nRepository Micro-Map (Defined API structures/schemas):\n%s", e.projCtx.RepoMap))
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
	return e.RunMessageWithStream(ctx, api.Message{Role: "user", Synthetic: true, Content: userMessage}, onDelta, nil)
}

// Steer injects user guidance into the running agent loop without interrupting.
// Thread-safe: callable from UI goroutine while RunMessageWithStream is blocking.
// The text is appended to the last tool result before the next LLM call, so the
// model sees the guidance at its next iteration.
func (e *Engine) Steer(text string) {
	if text == "" {
		return
	}
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	if e.pendingSteer != "" {
		e.pendingSteer += "\n" + text
	} else {
		e.pendingSteer = text
	}
}

func (e *Engine) drainPendingSteer() string {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	s := e.pendingSteer
	e.pendingSteer = ""
	return s
}

func (e *Engine) RunMessageWithStream(ctx context.Context, userMessage api.Message, onDelta func(delta string), onReasoning func(reasoning string)) (string, error) {
	if e.costTracker.OverBudget() {
		return "", fmt.Errorf("budget exceeded: %s", e.costTracker.Summary())
	}

	// Automatically re-collect project context, git status, file changes, and AST repo-map
	// right before running user query to keep the LLM completely synchronized with local file changes.
	if e.projCtx != nil {
		e.projCtx = ctxt.Collect()
		e.systemPrompt = "" // Invalidate the compiled system prompt to force reconstruction with the fresh context
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

	// Reset loop detector at the start of each turn
	if e.loopDetector != nil {
		e.loopDetector.Reset()
	}
	// Snapshot session for change tracking this turn
	e.sessionView = session.NewSessionView(e.messages, e.totalTokens)

	// Scan user input for safety issues (injection, secrets)
	if e.safetyChecker != nil {
		if result := e.safetyChecker.Scan(userMessage.Content, "user_input"); result != nil {
			if blocking := result.BlockingFinding(); blocking != nil {
				e.engineOutput(fmt.Sprintf("  \x1b[31m⚠ safety: %s\x1b[0m", blocking.Message))
				// Warn but don't block — user input is from the actual user
				log.Warnf("safety finding in user input: %s", blocking.Message)
			}
		}
	}

	// Route the user message to determine which model to use
	routedModel := e.config.Model // default fallback
	if e.modelRouter != nil {
		decision := e.modelRouter.Route(ctx, userMessage.Content)
		routedModel = decision.Model
		log.Debugf("model routing: %s (source=%s, reason=%s)", decision.Model, decision.Source, decision.Reason)
	}

	for iter := 0; iter < MaxIterations; iter++ {
		e.iterCount = iter + 1
		// Bail out immediately if the context has been cancelled (e.g. user pressed Ctrl+C)
		if ctx.Err() != nil {
			e.messages = prevMessages
			e.drainPendingSteer() // discard pending steer on cancel
			e.saveSession()
			return "", ctx.Err()
		}
		log.Debugf("agent iter=%d msgs=%d tokens=%d tools=%d model=%s cost=%s",
			iter, len(e.messages), e.totalTokens, len(toolDefs), e.config.Model, e.costTracker.Summary())

		// Drain pending steer: inject user guidance into the last tool message
		// so the model sees it on this iteration (matches Hermes /steer pattern).
		// If no tool message exists yet, put the steer back so the next
		// iteration (after tool execution) can inject it.
		if steer := e.drainPendingSteer(); steer != "" {
			injected := false
			for si := len(e.messages) - 1; si >= 0; si-- {
				if e.messages[si].Role == "tool" {
					e.messages[si].Content += "\n\n[用户指引] " + steer
					injected = true
					break
				}
			}
			if !injected {
				// No tool message yet �� put it back
				e.Steer(steer)
			}
		}

		// Compress message history if approaching context limits
		e.checkAndCompress(ctx)

		// Apply prompt cache breakpoints for Anthropic
		reqMessages := e.messages
		if e.fallback.Current().Name() == "anthropic" {
			reqMessages = api.InjectCacheBreakpoints(e.messages)
		}

		modelName := routedModel
		if modelName == "" {
			modelName = e.fallback.CurrentModel()
		}
		req := api.ChatRequest{
			Model:      modelName,
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
			walker = repl.NewWalkingIndicator("˼����...")
			walker.Start()
		}

		if useStream {
			firstDelta := true
			modelAct := e.beginActivity("����ģ�� " + e.fallback.CurrentModel())
			resp, _, err = e.fallback.TryChatStream(ctx, func(p api.Provider) api.ChatRequest { return req }, func(ev api.StreamEvent) {
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
			modelAct := e.beginActivity("����ģ�� " + e.fallback.CurrentModel())
			resp, _, err = e.fallback.TryChat(ctx, func(p api.Provider) api.ChatRequest { return req })
			e.endActivity(modelAct)
		}

		if walker != nil {
			walker.Stop()
		}

		if err != nil {
			e.messages = prevMessages
			e.saveSession()
			diagnostic.RecordRuntime(diagnostic.SevError, diagnostic.CatAPI,
				fmt.Sprintf("ģ�͵���ʧ��: %s", err.Error()))
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
			e.messages = append(e.messages, newSyntheticUserMsg("[system: your previous response was truncated due to length. Please continue, writing one file at a time.]"))
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

		// Next-speaker prediction: check if the model signals task completion
		// Also enforces max iterations as a safety net
		if e.nextSpeaker != nil {
			if e.iterCount >= MaxIterations-5 {
				e.engineOutput(fmt.Sprintf("  \x1b[2m(approaching max iterations: %d/%d)\x1b[0m", e.iterCount, MaxIterations))
			}
			if len(resp.ToolCalls) == 0 && !e.nextSpeaker.ShouldContinue(e.messages) {
				e.engineOutput("  \x1b[2m(model indicates task complete)\x1b[0m")
				break
			}
		}

		// Loop detection (enhanced 3-layer, P0-1).
		// Layer 1a: exact tool-call fingerprint in sliding window (14/10 for non-fast, 12/8 for fast).
		// Layer 1b: fuzzy tool+param pattern in sliding window (12/9 for non-fast, 10/7 for fast).
		// Layer 2: output content hash in sliding window (40/8 for non-fast, 30/8 for fast).
		// Layer 3: stagnation detection after N iterations without file activity.
		loopFp := e.fingerprintToolCalls(resp.ToolCalls)
		if e.loopDetector != nil {
			if lr := e.loopDetector.RecordToolCalls(loopFp); lr.Detected {
				log.Warnf("loop detected (layer %d): %s", lr.Layer, lr.Reason)
				if lr.Fatal {
					e.engineOutput("? " + lr.Reason)
					return "", fmt.Errorf("loop detection: %s", lr.Reason)
				}
				// Non-fatal: inject guidance asking the model to change approach
				e.messages = append(e.messages, newSyntheticUserMsg(injectLoopGuidance(lr.Reason)))
				// Reset fingerprint history so the model gets a fresh start
				// after seeing the guidance, preventing old history from
				// immediately triggering another detection.
				e.loopDetector.ResetFingerprintHistory()
			}
		} else {
			// Fallback: simple loop detection (kept for backward compatibility)
			e.loopHistory = append(e.loopHistory, loopFp)
			if len(e.loopHistory) > 10 {
				e.loopHistory = e.loopHistory[1:]
			}
			if loopFp != "" && e.countRecent(loopFp, 5) >= 3 {
				log.Warnf("loop detected: %s", loopFp)
				e.messages = append(e.messages, newSyntheticUserMsg("[system: 检测到重复循环 — 模型连续多次调用相同的工具和参数。请尝试完全不同的方法，如果卡住了可以向用户寻求帮助]"))
				e.loopHistory = nil // reset after injecting guidance
			}
		}

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
					e.engineOutput(fmt.Sprintf("\r  \x1b[2m? [%s]...\x1b[0m", tc.Name))
				}
				res := e.executeTool(ctx, tc)
				results[i] = toolResult{ID: tc.ID, Name: tc.Name, Content: res}
			}
		}

		for _, r := range results {
			isErr := strings.HasPrefix(r.Content, "Error:")
			if !e.config.Debug {
				e.engineOutput(fmt.Sprintf("\r\x1b[K%s\n", formatToolLine(r.Name, summarizeResult(r.Content), isErr)))
			}
			// Session notes capture (always, regardless of debug mode)
			if e.sessionNotes != nil {
				if isErr {
					e.sessionNotes.AddError(fmt.Sprintf("%s: %s", r.Name, summarizeResult(r.Content)))
				}
			}
			if isErr {
				diagnostic.RecordRuntime(diagnostic.SevWarning, diagnostic.CatTool,
					fmt.Sprintf("���� %s ʧ��: %s", r.Name, summarizeResult(r.Content)))
			}
			e.messages = append(e.messages, api.Message{
				Role: "tool", ToolCallID: r.ID, Name: r.Name, Content: r.Content,
			})
			// Feed loop detector with tool output (Layer 2: content hash)
			if e.loopDetector != nil && !isErr {
				if lr := e.loopDetector.RecordOutput(r.Content); lr.Detected {
					log.Warnf("loop detected (layer 2): %s", lr.Reason)
					if lr.Fatal {
						e.engineOutput("? " + lr.Reason)
						return "", fmt.Errorf("loop detection: %s", lr.Reason)
					}
					// Non-fatal: inject guidance asking the model to change approach
					e.messages = append(e.messages, newSyntheticUserMsg(injectLoopGuidance(lr.Reason)))
					// Reset fingerprint history so the model gets a fresh start
					e.loopDetector.ResetFingerprintHistory()
				}
			}
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
		// Compression is handled by checkAndCompress at iteration start (line ~465).
		// Record iteration for stagnation detection (Layer 3).
		// L3 is log-only — no file activity doesn't mean the model is stuck
		// (research, reading, search are legitimate non-file workflows).
		if e.loopDetector != nil {
			if lr := e.loopDetector.RecordIteration(); lr.Detected {
				log.Warnf("stagnation (layer 3): %s", lr.Reason)
				if lr.Fatal {
					e.engineOutput("? " + lr.Reason)
					return "", fmt.Errorf("loop detection: %s", lr.Reason)
				}
				// L3 is a weak signal — log only, don't inject guidance.
				// The model may be doing legitimate research/reading.
			}
		}
	}

	e.drainPendingSteer() // discard pending steer on max iterations
	return "", fmt.Errorf("max iterations (%d) reached, cost: %s", MaxIterations, e.costTracker.Summary())
}

func (e *Engine) executeTool(ctx context.Context, tc api.ToolCall) string {
	t, ok := e.registry.Find(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}

	// Run safety checks before executing the tool
	if e.safetyChecker != nil {
		result := e.safetyChecker.ScanToolCall(tc.Name, tc.Input)
		if blocking := result.BlockingFinding(); blocking != nil {
			e.engineOutput(fmt.Sprintf("  \x1b[31m✗ blocked: %s\x1b[0m", blocking.Message))
			return fmt.Sprintf("BLOCKED by safety checker: %s", blocking.Message)
		}
	}

	// Track this tool as an in-flight stage so a hung tool (e.g. a bash command
	// or MCP call that ignores ctx) is attributable by the stall monitor.
	toolAct := e.beginActivity("ִ�й��� " + tc.Name)
	defer e.endActivity(toolAct)

	// Fire pre-tool-use hooks
	if e.hookMgr != nil {
		e.hookMgr.FireLegacy(ctx, hooks.PreToolUse, hooks.ToolUseInfo{
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
			// Policy engine override: check rules before interactive prompt
			if e.policyEngine != nil {
				action := e.policyEngine.Evaluate(tc.Name, tc.Input, e.config.PermissionMode)
				if action == permission.ActionAllow {
					decision = permission.DAllow // skip interactive prompt
				} else if action == permission.ActionDeny {
					return fmt.Sprintf("Error: denied by policy for %s", tc.Name)
				}
			}
			if decision == permission.DAsk && e.PermissionPrompt != nil {
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
		e.hookMgr.FireLegacy(ctx, hooks.PostToolUse, hooks.ToolUseInfo{
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
			// Notify loop detector of file activity (Layer 3 stagnation tracking)
			if e.loopDetector != nil {
				e.loopDetector.RecordFileActivity(path, tc.Name == "write")
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

// Compact compresses the message history on demand (e.g. via /compact command).
// Delegates to the ChatCompressor's two-layer pipeline.
func (e *Engine) Compact(ctx context.Context) {
	e.compactIfNeeded(ctx)
}

// checkAndCompress runs the compressor at the start of each iteration as a lightweight guard.
func (e *Engine) checkAndCompress(ctx context.Context) {
	// Mask old tool outputs before compression to reduce tokens
	if e.masker != nil {
		_, maskedMsgs := e.masker.Mask(e.messages, nil)
		e.messages = maskedMsgs
		e.totalTokens = countTokens(e.messages)
	}

	if e.compressor == nil {
		return
	}
	if !e.compressor.NeedsCompression(e.totalTokens, CompactTokenThreshold) {
		return
	}
	e.compactIfNeeded(ctx)
}

// compactIfNeeded runs the full two-layer compression pipeline.
func (e *Engine) compactIfNeeded(ctx context.Context) {
	if e.compressor == nil {
		return
	}
	if e.sessionNotes != nil {
		e.sessionNotes.AddDecision(fmt.Sprintf("Context compacted at %d tokens, %d messages", e.totalTokens, len(e.messages)))
	}
	
	// Use model_fast for compression summaries — much cheaper than the main model.
	// Falls back to the main model if model_fast is not configured.
	tryChat := func(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
		if e.config.ModelFast != "" {
			req.Model = e.config.ModelFast
		}
		resp, _, err := e.fallback.TryChat(ctx, func(p api.Provider) api.ChatRequest { return req })
		return resp, err
	}
	
	result, newMsgs := e.compressor.Compress(ctx, e.messages, e.totalTokens, CompactTokenThreshold, tryChat)
	if result.Compressed {
		e.messages = newMsgs
		e.totalTokens = countTokens(e.messages)
		log.Debugf("agent compacted: %d tokens/%d msgs -> %d tokens/%d msgs",
			result.OldCount, result.NewCount, e.totalTokens, len(e.messages))
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
	// Auto-set title from first real user message
	if len(e.messages) > 0 && (e.session.Title == "New session" || e.session.Title == "") {
		if title := pickSessionTitle(e.messages); title != "" {
			e.session.Title = title
		}
	}
	e.store.Save(e.session)
}

// isSyntheticUserMessage returns true if a user-role message was injected
// by the engine (loop guidance, compression summary, circuit breaker, etc.)
// rather than authored by the actual user. These should never be used as
// session titles or history previews.
func isSyntheticUserMessage(content string) bool {
	c := strings.TrimSpace(content)
	if c == "" {
		return true
	}
	// Engine-injected prefixes
	syntheticPrefixes := []string{
		"[system:",                     // circuit breaker
		"[Conversation Summary]",       // AI compression
		"[系统检测到重复操作循环]",        // loop guidance (Chinese)
		"[Context truncated",           // truncation notice
		"[用户指引]",                    // steer guidance
		"[Continue the task",           // compression continuation
		"[会话摘要]",                    // old compression (Chinese)
	}
	for _, p := range syntheticPrefixes {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// newSyntheticUserMsg creates a user-role message marked as engine-injected,
// ensuring it won't be used as a session title or history preview.
func newSyntheticUserMsg(content string) api.Message {
	return api.Message{Role: "user", Content: content, Synthetic: true}
}

// pickSessionTitle returns the first real (non-synthetic) user message
// as the session title, truncated to 60 chars. Returns "" if no valid message found.
// looksSynthetic checks if a user message is engine-injected.
// Primary check: Synthetic flag (new messages).
// Fallback: content prefix matching (old sessions from before Synthetic was added).
func looksSynthetic(m api.Message) bool {
	if m.Synthetic {
		return true
	}
	// Backward-compatible: old sessions don't have Synthetic flag.
	// Check content for known engine-injected prefixes.
	c := strings.TrimSpace(m.Content)
	knownPrefixes := []string{
		"[system:",
		"[Conversation Summary]",
		"[系统检测到重复操作循环]",
		"[Context truncated",
		"[用户指引]",
		"[Continue the task",
		"[会话摘要]",
		"run slow tool",
		"do something",
		"slow response",
	}
	for _, p := range knownPrefixes {
		if strings.HasPrefix(c, p) || strings.EqualFold(c, p) {
			return true
		}
	}
	return false
}

func pickSessionTitle(messages []api.Message) string {
	for _, m := range messages {
		if m.Role == "user" && !looksSynthetic(m) && strings.TrimSpace(m.Content) != "" {
			text := strings.TrimSpace(m.Content)
			if len(text) > 60 {
				text = text[:60] + "..."
			}
			return text
		}
	}
	return ""
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
		return fmt.Sprintf("  %s?%s %s[%s]%s %s%s%s", red, reset, red, name, reset, red, summary, reset)
	}
	return fmt.Sprintf("  %s?%s %s[%s]%s %s%s%s", green, reset, cyan, name, reset, dim, summary, reset)
}

// runTurnEndPipeline executes quiet post-turn persistence only.
func (e *Engine) runTurnEndPipeline() {
	// Capture session diff for change tracking
	if e.sessionView != nil {
		currentView := session.NewSessionView(e.messages, e.totalTokens)
		if diff := session.Diff(e.sessionView, currentView); diff.HasChanges() {
			summary := diff.Summary()
			log.Debugf("session changes: %s", summary)
			if len(diff.AddedFiles) > 0 || len(diff.AddedTools) > 0 {
				e.engineOutput(fmt.Sprintf("  \x1b[2m📊 %s\x1b[0m", summary))
			}
		}
		e.sessionView = currentView
	}
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

// engineOutput emits a diagnostic line to the registered callback,
// or falls back to stderr.
func (e *Engine) engineOutput(line string) {
	if e.OnEngineOutput != nil {
		e.OnEngineOutput(line)
		return
	}
	fmt.Fprintf(os.Stderr, "%s", line)
}

// WirePlanExecutor sets up the PlanExecuteFunc on the runtime so the
// execute_plan tool can decompose and run multi-step plans.
// It uses the engine's own provider to power sub-agents.
// When the provider is unavailable (no API key configured),
// the function is still set but returns a guidance message for the LLM.
func (e *Engine) WirePlanExecutor() {
	if e.runtime == nil {
		return
	}

	if e.fallback != nil && e.fallback.Current() != nil {
		d := delegate.NewDelegator(e.fallback.Current(), e.fallback.CurrentModel(), e.registry.All())
		pe := plan.NewPlanExecutor(d, e.runtime)
		e.runtime.PlanExecuteFunc = func(parallel bool) (string, error) {
			pl, err := plan.FromRuntime("plan", e.runtime)
			if err != nil {
				return "", err
			}
			pl.Parallel = parallel
			result := pe.Execute(context.Background(), pl)
			return plan.FormatResult(result), nil
		}
		return
	}

	// No provider available: register a fallback that guides the LLM
	// to execute tasks sequentially without sub-agents.
	e.runtime.PlanExecuteFunc = func(parallel bool) (string, error) {
		return "No API provider configured. Execute tasks one at a time " +
			"using available tools (read, write, bash, etc.) instead of " +
			"sub-agents. Follow the todowrite plan sequentially.", nil
	}
}

// fingerprintToolCalls creates a stable, compact fingerprint from a set of
// tool calls for loop detection. It joins tool names and key argument values.
func (e *Engine) fingerprintToolCalls(toolCalls []api.ToolCall) string {
	if len(toolCalls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		// Include the tool name and the first non-empty value from well-known keys
		key := tc.Name
		for _, k := range []string{"filePath", "command", "pattern", "query", "url", "name", "title", "message"} {
			if v, ok := tc.Input[k].(string); ok && v != "" {
				key += ":" + v
				break
			}
		}
		parts = append(parts, key)
	}
	// Sort to make the fingerprint order-independent
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// isFastModelName checks if a model name indicates a fast/flash/cheap model
// that is more prone to repetitive loops and needs tighter detection thresholds.
func isFastModelName(model string) bool {
	model = strings.ToLower(model)
	fastIndicators := []string{"flash", "mini", "lite", "tiny", "fast", "haiku", "nano"}
	for _, ind := range fastIndicators {
		if strings.Contains(model, ind) {
			return true
		}
	}
	return false
}

// countRecent counts how many times the fingerprint appears in the last window
// entries of the loop history.
func (e *Engine) countRecent(fp string, window int) int {
	start := len(e.loopHistory) - window
	if start < 0 {
		start = 0
	}
	count := 0
	for _, h := range e.loopHistory[start:] {
		if h == fp {
			count++
		}
	}
	return count
}

