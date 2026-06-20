// Package covemobile exposes a lightweight AI engine for Android phone control.
// It wraps the multi-provider AI layer and a tool-calling loop that delegates
// phone operations (tap, swipe, screenshot) to the Kotlin side via callback.
package covemobile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/mobile/mobileapi"
)

// ---------- exported types for gomobile / Kotlin ----------
// ToolDef describes a phone-operation tool available to the AI.
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
	provider    mobileapi.Provider
	model       string
	messages    []mobileapi.Message
	toolDefs    []ToolDef
	initialized bool
}

// Init initializes the engine with provider config. Must be called after New().
func (e *MobileEngine) Init(apiKey string, model string, provider string, baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	providerCfg := mobileapi.ProviderConfig{
		Name:    provider,
		APIKey:  apiKey,
		BaseURL: baseURL,
	}
	prov := mobileapi.DetectProvider(model, providerCfg)

	e.provider = prov
	e.model = model
	e.messages = make([]mobileapi.Message, 0)
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

// Reset clears conversation history.
func (e *MobileEngine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.messages = make([]mobileapi.Message, 0)
}

// ChatStream sends a message and streams the response via callback.
// Handles the tool-calling loop: when AI calls a tool, the callback is invoked,
// the result is fed back to the AI, and the loop continues until a final text response.
func (e *MobileEngine) ChatStream(message string, timeoutSecs int, callback StreamCallback) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
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
	e.messages = append(e.messages, mobileapi.Message{Role: "user", Content: message})

	toolDefs := e.buildAPIToolDefs()
	sp := e.buildSystemPrompt()
	prov := e.provider
	model := e.model
	msgs := make([]mobileapi.Message, len(e.messages))
	copy(msgs, e.messages)
	e.mu.Unlock()

	req := mobileapi.ChatRequest{
		Model:      model,
		Messages:   msgs,
		SystemBase: sp,
		Tools:      toolDefs,
		MaxTokens:  8192,
	}

	fullResponse := &strings.Builder{}

	for iter := 0; iter < 30; iter++ {
		if ctx.Err() != nil {
			callback.OnError("timeout")
			return
		}

		resp, err := prov.ChatStream(ctx, req, func(event mobileapi.StreamEvent) {
			if event.Delta != "" {
				callback.OnDelta(event.Delta)
			}
			if event.Reasoning != "" {
				callback.OnReasoning(event.Reasoning)
			}
		})
		if err != nil {
			callback.OnError(fmt.Sprintf("API error: %v", err))
			return
		}

		if resp.Content != "" {
			fullResponse.WriteString(resp.Content)
		}
		if resp.ReasoningContent != "" {
			callback.OnReasoning(resp.ReasoningContent)
		}

		if len(resp.ToolCalls) > 0 {
			// Add assistant message with tool calls to history
			assistantMsg := mobileapi.Message{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: make([]mobileapi.ToolCall, len(resp.ToolCalls)),
			}
			for i, tc := range resp.ToolCalls {
				assistantMsg.ToolCalls[i] = mobileapi.ToolCall{
					ID: tc.ID, Name: tc.Name, Input: tc.Input,
				}
			}
			e.mu.Lock()
			e.messages = append(e.messages, assistantMsg)
			e.mu.Unlock()

			// Execute each tool via Kotlin callback, collect results
			for _, tc := range resp.ToolCalls {
				inputJSON, _ := json.Marshal(tc.Input)
				result := callback.OnToolCall(tc.Name, string(inputJSON))

				e.mu.Lock()
				e.messages = append(e.messages, mobileapi.Message{
					Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: result,
				})
				e.mu.Unlock()
			}

			// Update request with full history for next LLM iteration
			e.mu.Lock()
			req.Messages = make([]mobileapi.Message, len(e.messages))
			copy(req.Messages, e.messages)
			e.mu.Unlock()
			continue
		}

		// Final text response - no tool calls
		e.mu.Lock()
		e.messages = append(e.messages, mobileapi.Message{Role: "assistant", Content: resp.Content})
		e.mu.Unlock()

		callback.OnDone(fullResponse.String())
		return
	}

	callback.OnError("max iterations reached")
}

// ---------- internal helpers ----------

func (e *MobileEngine) buildAPIToolDefs() []mobileapi.ToolDef {
	defs := make([]mobileapi.ToolDef, 0, len(e.toolDefs))
	for _, td := range e.toolDefs {
		schema := map[string]any{"type": "object", "properties": map[string]any{}}
		if td.InputSchema != "" {
			if err := json.Unmarshal([]byte(td.InputSchema), &schema); err != nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
		}
		defs = append(defs, mobileapi.ToolDef{
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
			sb.WriteString(fmt.Sprintf("- %s: %s\n", td.Name, td.Description))
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
