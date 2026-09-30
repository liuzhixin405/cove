// Package covemobile exposes a lightweight AI engine for Android phone control.
// It wraps the desktop's provider layer (internal/api) and a tool-calling loop
// that delegates phone operations (tap, swipe, screenshot) to the Kotlin side
// via callback.
//
// gomobile bind only restricts the EXPORTED API of this package (the types
// and signatures below must stay gomobile-bindable and unchanged for the
// Kotlin side); the implementation may import any package of the module.
// Using internal/api directly means mobile gets the same tool-argument JSON
// repair, connection-setup retry, SSE handling and Anthropic support as the
// desktop instead of a drifting copy. Only the provider layer is shared: the
// desktop engine (internal/engine: tools, permissions, compaction) is not
// part of the mobile build.
package covemobile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// ---------- exported types for gomobile / Kotlin ----------

// ToolDef describes a phone-operation tool available to the AI.
type ToolDef struct {
	Name        string
	Description string
	InputSchema string
}

// ---------- Callback interface (implemented in Kotlin) ----------

// StreamCallback receives streaming AI responses and executes phone tools.
type StreamCallback interface {
	OnDelta(delta string)
	OnToolCall(toolName string, inputJSON string) string
	OnDone(response string)
	OnReasoning(reasoning string)
	OnError(err string)
}

// ---------- MobileEngine ----------

// MobileEngine is the lightweight AI engine for Android phone control.
// No cost tracking, no non-streaming Chat - only what the phone needs.
type MobileEngine struct {
	mu          sync.Mutex
	provider    api.Provider
	model       string
	messages    []api.Message
	toolDefs    []ToolDef
	initialized bool
	// gen counts the conversations: Reset and Init start a new one. A
	// ChatStream remembers the generation it started in and drops its
	// results once that has changed, and genCancel stops the requests of
	// the current generation. Reset used to clear messages while a run went
	// on: its tool loop appended the tool results onto the fresh history,
	// which then began with orphan "tool" messages that every later request
	// was rejected for.
	gen       uint64
	genCtx    context.Context
	genCancel context.CancelFunc
}

// errReset is what a run cut short by Reset reports.
const errReset = "cancelled: conversation reset"

// newGenerationLocked ends the current conversation: its runs are cancelled
// and whatever they still produce is dropped. Callers hold e.mu.
func (e *MobileEngine) newGenerationLocked() {
	if e.genCancel != nil {
		e.genCancel()
	}
	e.gen++
	e.genCtx, e.genCancel = context.WithCancel(context.Background())
}

// generationLocked returns the current generation and its context, creating
// them for an engine that was never reset. Callers hold e.mu.
func (e *MobileEngine) generationLocked() (uint64, context.Context) {
	if e.genCtx == nil {
		e.genCtx, e.genCancel = context.WithCancel(context.Background())
	}
	return e.gen, e.genCtx
}

// appendIfCurrent appends msgs to the history when gen is still the current
// generation and reports whether it did.
func (e *MobileEngine) appendIfCurrent(gen uint64, msgs ...api.Message) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gen != gen {
		return false
	}
	e.messages = append(e.messages, msgs...)
	return true
}

// Init initializes the engine with provider config. Must be called after New().
func (e *MobileEngine) Init(apiKey string, model string, provider string, baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	prov := api.NewProvider(api.ProviderConfig{
		Name:    resolveProviderName(provider, model),
		APIKey:  apiKey,
		BaseURL: mobileBaseURL(provider, baseURL),
	})

	e.newGenerationLocked()
	e.provider = prov
	e.model = model
	e.messages = make([]api.Message, 0)
	e.toolDefs = make([]ToolDef, 0)
	e.initialized = true
}

// AddTool registers a single phone-operation tool (gomobile limitation: one at a time).
func (e *MobileEngine) AddTool(name string, description string, inputSchema string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolDefs = append(e.toolDefs, ToolDef{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
	})
}

// Reset clears conversation history and stops a ChatStream still running
// (it reports OnError with errReset).
func (e *MobileEngine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.newGenerationLocked()
	e.messages = make([]api.Message, 0)
}

// ChatStream sends a message and streams the response via callback.
// Handles the tool-calling loop: when AI calls a tool, the callback is invoked,
// the result is fed back to the AI, and the loop continues until a final text response.
func (e *MobileEngine) ChatStream(message string, timeoutSecs int, callback StreamCallback) {
	// timeoutSecs <= 0 means no deadline. It used to build an already-expired
	// context, so a caller passing 0 got "timeout" before any request was sent.
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if timeoutSecs > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	if callback == nil {
		return
	}

	e.mu.Lock()
	if !e.initialized {
		e.mu.Unlock()
		callback.OnError("engine not initialized, call Init() first")
		return
	}
	gen, genCtx := e.generationLocked()
	// Reset cancels genCtx, and with it this run's requests.
	stop := context.AfterFunc(genCtx, cancel)
	defer stop()
	e.messages = append(e.messages, api.Message{Role: "user", Content: message})

	toolDefs := e.buildAPIToolDefs()
	sp := e.buildSystemPrompt()
	prov := e.provider
	model := e.model
	msgs := make([]api.Message, len(e.messages))
	copy(msgs, e.messages)
	e.mu.Unlock()

	req := api.ChatRequest{
		Model:      model,
		Messages:   msgs,
		SystemBase: sp,
		Tools:      toolDefs,
		MaxTokens:  8192,
	}

	fullResponse := &strings.Builder{}

	// reset reports whether the conversation was reset under this run.
	reset := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.gen != gen
	}
	for iter := 0; iter < 30; iter++ {
		if reset() {
			callback.OnError(errReset)
			return
		}
		if ctx.Err() != nil {
			callback.OnError("timeout")
			return
		}

		resp, err := prov.ChatStream(ctx, req, func(event api.StreamEvent) {
			if event.Delta != "" {
				callback.OnDelta(event.Delta)
			}
			if event.Reasoning != "" {
				callback.OnReasoning(event.Reasoning)
			}
		})
		if err != nil {
			if reset() {
				callback.OnError(errReset)
				return
			}
			callback.OnError(fmt.Sprintf("API error: %v", err))
			return
		}
		if resp == nil {
			callback.OnError("API error: empty response")
			return
		}

		if resp.Content != "" {
			fullResponse.WriteString(resp.Content)
		}
		// resp.ReasoningContent is not sent again: it is the accumulation of
		// the pieces the stream callback above already delivered, and re-sending
		// it showed every thought twice.

		if len(resp.ToolCalls) > 0 {
			// Add assistant message with tool calls to history. The reasoning
			// and thinking blocks go with it: DeepSeek thinking mode requires
			// reasoning_content to be sent back on a tool-calling turn, and
			// Anthropic requires its thinking blocks back verbatim.
			assistantMsg := api.Message{
				Role:             "assistant",
				Content:          resp.Content,
				ReasoningContent: resp.ReasoningContent,
				ThinkingBlocks:   resp.ThinkingBlocks,
				ToolCalls:        make([]api.ToolCall, len(resp.ToolCalls)),
			}
			copy(assistantMsg.ToolCalls, resp.ToolCalls)
			if !e.appendIfCurrent(gen, assistantMsg) {
				callback.OnError(errReset)
				return
			}

			// Execute each tool via Kotlin callback, collect results
			for _, tc := range resp.ToolCalls {
				result := runTool(tc, callback)
				if !e.appendIfCurrent(gen, api.Message{
					Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: result,
				}) {
					callback.OnError(errReset)
					return
				}
			}

			// Update request with full history for next LLM iteration
			e.mu.Lock()
			req.Messages = make([]api.Message, len(e.messages))
			copy(req.Messages, e.messages)
			e.mu.Unlock()
			continue
		}

		// Final text response - no tool calls
		if !e.appendIfCurrent(gen, api.Message{Role: "assistant", Content: resp.Content}) {
			callback.OnError(errReset)
			return
		}

		callback.OnDone(fullResponse.String())
		return
	}

	callback.OnError("max iterations reached")
}

// ---------- internal helpers ----------

// resolveProviderName keeps the mobile engine's provider selection for an
// empty provider name: claude models go to Anthropic, deepseek models to
// DeepSeek and everything else to OpenAI. (api.DetectProvider falls back to
// Anthropic for an unknown model, which would change what an app that only
// sets a model gets.) A non-empty name is passed through; api.NewProvider
// normalizes aliases and treats unknown names as OpenAI-compatible.
// legacyMobileBaseURL is where the mobile client this package replaced sent
// a provider it had no address for.
const legacyMobileBaseURL = "https://api.deepseek.com/v1"

// mobileBaseURL keeps the address an installed app's settings point at. The
// old client sent "openai-compatible", or a provider name it did not know,
// to DeepSeek when no base URL was set; internal/api would send them to
// OpenAI, and such an app (typically a DeepSeek key under
// "openai-compatible") stopped working after an update. Known providers get
// their own address, which the old client got wrong (glm went to DeepSeek).
func mobileBaseURL(provider, baseURL string) string {
	if strings.TrimSpace(baseURL) != "" || strings.TrimSpace(provider) == "" {
		return baseURL
	}
	if api.NormalizeProviderName(provider) == "openai-compatible" || !api.IsKnownProvider(provider) {
		return legacyMobileBaseURL
	}
	return baseURL
}

func resolveProviderName(provider, model string) string {
	if strings.TrimSpace(provider) != "" {
		return provider
	}
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.Contains(m, "deepseek"):
		return "deepseek"
	default:
		return "openai"
	}
}

// runTool executes one model tool call through the Kotlin callback. A call
// whose arguments internal/api could not parse even after JSON repair, or one
// without a tool name, is answered with an error for the model instead of
// dispatching garbage to the phone (same contract as the desktop engine).
func runTool(tc api.ToolCall, callback StreamCallback) string {
	if tc.ParseError {
		msg, _ := tc.Input["_cove_parse_error"].(string)
		if msg == "" {
			msg = "tool call arguments could not be parsed as JSON"
		}
		return fmt.Sprintf("Error: %s. Please resend this tool call with valid JSON arguments (check quote escaping, and avoid truncating long string fields).", msg)
	}
	if strings.TrimSpace(tc.Name) == "" {
		return "Error: tool call has no tool name. Please resend it naming one of the available tools."
	}
	input := tc.Input
	if input == nil {
		input = map[string]any{}
	}
	inputJSON, _ := json.Marshal(input)
	return callback.OnToolCall(tc.Name, string(inputJSON))
}

func (e *MobileEngine) buildAPIToolDefs() []api.ToolDef {
	defs := make([]api.ToolDef, 0, len(e.toolDefs))
	for _, td := range e.toolDefs {
		schema := map[string]any{"type": "object", "properties": map[string]any{}}
		if td.InputSchema != "" {
			if err := json.Unmarshal([]byte(td.InputSchema), &schema); err != nil || schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
		}
		defs = append(defs, api.ToolDef{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: schema,
		})
	}
	return defs
}

func (e *MobileEngine) buildSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are an AI assistant. You can help with general questions, chat, and tasks.\n\n")
	if len(e.toolDefs) > 0 {
		sb.WriteString("Available tools:\n")
		for _, td := range e.toolDefs {
			fmt.Fprintf(&sb, "- %s: %s\n", td.Name, td.Description)
		}
		sb.WriteString("\nGuidelines:\n")
		sb.WriteString("- Only use tools when explicitly needed for the task.\n")
		sb.WriteString("- For general conversation, just reply naturally without tools.\n")
		sb.WriteString("- One tool call per response.\n")
	} else {
		sb.WriteString("No tools are available. Just reply naturally to the user.\n")
	}
	return sb.String()
}
