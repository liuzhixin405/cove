package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Gemini 3's thought signature arrives in a tool call's extra_content and
// must go back with that call in the next request; the API answered 400
// "Function call is missing a thought_signature" when it was dropped.
func TestThoughtSignatureRoundTrip(t *testing.T) {
	const extra = `{"google":{"thought_signature":"c2lnbmF0dXJl"}}`
	resp, _, err := streamOpenAI(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"extra_content\":"+extra+",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
		"data: [DONE]\n\n")
	if err != nil {
		t.Fatal(err)
	}
	p := &openAICompatProvider{apiKey: "k"}
	if len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Extra) != extra {
		t.Fatalf("tool calls = %+v, want the extra_content kept", resp.ToolCalls)
	}

	// The next request carries it back, and survives a session save.
	data, _ := json.Marshal(resp.ToolCalls[0])
	var restored ToolCall
	if err := json.Unmarshal(data, &restored); err != nil || string(restored.Extra) != extra {
		t.Fatalf("persisted tool call lost the signature: %s", data)
	}
	msgs := p.convertMessages([]Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{restored}},
		{Role: "tool", ToolCallID: "c1", Content: "a"},
	})
	out, _ := json.Marshal(msgs)
	if !strings.Contains(string(out), `"extra_content":`+extra) {
		t.Fatalf("request lacks the signature: %s", out)
	}
}
