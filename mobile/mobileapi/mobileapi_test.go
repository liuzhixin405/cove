package mobileapi

import (
	
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildMessages(t *testing.T) {
	// 测试普通消息序列化
	req := ChatRequest{
		Model: "deepseek-chat",
		Messages: []Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Hello"},
		},
	}
	
	// 手动构造消息来验证格式
	messages := buildTestMessages(req)
	
	data, _ := json.MarshalIndent(messages, "", "  ")
	t.Logf("Messages JSON:\n%s", string(data))
	
	// 验证 system 消息
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0]["role"] != "system" {
		t.Errorf("expected system role, got %v", messages[0]["role"])
	}
	if messages[0]["content"] != "You are a helpful assistant." {
		t.Errorf("expected 'You are a helpful assistant.', got %v", messages[0]["content"])
	}
}

func TestBuildMessagesWithToolCalls(t *testing.T) {
	// 测试带 tool_calls 的消息序列化
	req := ChatRequest{
		Model: "deepseek-chat",
		Messages: []Message{
			{Role: "user", Content: "What's on my screen?"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_123",
						Name: "screenshot",
						Input: map[string]any{},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_123",
				Name:       "screenshot",
				Content:    "Screen content: Hello World button at (100, 200)",
			},
		},
	}
	
	messages := buildTestMessages(req)
	
	data, _ := json.MarshalIndent(messages, "", "  ")
	t.Logf("Tool call messages JSON:\n%s", string(data))
	
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	
	// 验证 assistant 消息有 tool_calls
	assistant := messages[1]
	if assistant["role"] != "assistant" {
		t.Errorf("expected assistant role, got %v", assistant["role"])
	}
	tcs := assistant["tool_calls"].([]map[string]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0]
	if tc["id"] != "call_123" {
		t.Errorf("expected call_123, got %v", tc["id"])
	}
	
	// 验证 tool 消息有 tool_call_id
	tool := messages[2]
	if tool["role"] != "tool" {
		t.Errorf("expected tool role, got %v", tool["role"])
	}
	if tool["tool_call_id"] != "call_123" {
		t.Errorf("expected call_123, got %v", tool["tool_call_id"])
	}
}

func TestBuildMessagesWithTools(t *testing.T) {
	// 测试带 tools 定义的请求
	req := ChatRequest{
		Model: "deepseek-chat",
		Messages: []Message{
			{Role: "user", Content: "Click the button"},
		},
		Tools: []ToolDef{
			{
				Name:        "tap",
				Description: "Tap at coordinates",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"x": map[string]any{"type": "number"},
						"y": map[string]any{"type": "number"},
					},
					"required": []string{"x", "y"},
				},
			},
		},
		MaxTokens: 8192,
	}
	
	// 验证序列化
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	t.Logf("ChatRequest JSON:\n%s", string(data))
	
	if !strings.Contains(string(data), `"tap"`) {
		t.Error("expected tool name 'tap' in JSON")
	}
	if !strings.Contains(string(data), `"x"`) {
		t.Error("expected parameter 'x' in JSON")
	}
}

func TestParseToolCallChunks(t *testing.T) {
	// 测试流式 tool call 解析（模拟分块传输）
	accum := ""
	chunks := []string{`{"x":`, `100,`, `"y":`, `200}`, ``}
	
	for _, chunk := range chunks {
		accum += chunk
	}
	
	var parsed map[string]any
	if err := json.Unmarshal([]byte(accum), &parsed); err != nil {
		t.Fatalf("failed to parse accumulated args '%s': %v", accum, err)
	}
	
	if parsed["x"] != float64(100) {
		t.Errorf("expected x=100, got %v", parsed["x"])
	}
	if parsed["y"] != float64(200) {
		t.Errorf("expected y=200, got %v", parsed["y"])
	}
	
	t.Logf("Parsed args: x=%v, y=%v", parsed["x"], parsed["y"])
}

// buildTestMessages 复制 ChatStream 中的消息构建逻辑用于测试
func buildTestMessages(req ChatRequest) []map[string]interface{} {
	var messages []map[string]interface{}

	if req.SystemBase != "" || req.System != "" {
		combined := req.SystemBase
		if combined != "" && req.System != "" {
			combined += "\n\n"
		}
		combined += req.System
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": combined,
		})
	}

	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]interface{}
			for _, tc := range m.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Input)
				tcs = append(tcs, map[string]interface{}{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      tc.Name,
						"arguments": string(argsJSON),
					},
				})
			}
			messages = append(messages, map[string]interface{}{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": tcs,
			})
			continue
		}

		if m.Role == "tool" {
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      m.Content,
			})
			continue
		}

		messages = append(messages, map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		})
	}
	return messages
}