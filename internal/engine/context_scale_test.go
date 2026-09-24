package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

func toolHeavyHistory(n, charsEach int) []api.Message {
	msgs := []api.Message{{Role: "user", Content: "go"}}
	for i := 0; i < n; i++ {
		id := string(rune('a' + i%26))
		msgs = append(msgs,
			api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: id, Name: "read"}}},
			api.Message{Role: "tool", ToolCallID: id, Name: "read", Content: strings.Repeat("x", charsEach)})
	}
	return append(msgs, api.Message{Role: "user", Content: "continue"})
}

func maskedCount(msgs []api.Message) int {
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, maskedPrefix) {
			n++
		}
	}
	return n
}

// Old tool output was swapped for "saved to disk" placeholders once ~80K tokens
// of history existed, whatever the model's window. On a 1M-token model that
// made the agent re-read files it had already read, long before any limit.
func TestLargeWindowModelKeepsOldToolOutputs(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.masker.outputDir = t.TempDir()
	eng.compressor = nil
	eng.messages = toolHeavyHistory(30, 16000) // ~120K tokens of tool output
	eng.checkAndCompress(context.Background(), "deepseek-v4-pro")
	if n := maskedCount(eng.messages); n != 0 {
		t.Fatalf("%d tool outputs masked on a 1M-token model with ~120K tokens of history", n)
	}
}

func TestSmallWindowModelStillMasksOldToolOutputs(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.masker.outputDir = t.TempDir()
	eng.compressor = nil
	eng.messages = toolHeavyHistory(30, 16000)
	eng.checkAndCompress(context.Background(), "qwen-turbo") // 32K window
	if maskedCount(eng.messages) == 0 {
		t.Fatal("a 32K-window model must still mask old tool output")
	}
}

func readTurnOutput(t *testing.T, model string, size int) string {
	t.Helper()
	read := &mockTool{name: "read", readOnly: true, safe: true, result: strings.Repeat("y", size)}
	prov := &seqProvider{reply: func(ctx context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("r1", "read", map[string]any{"filePath": "big.go"}), nil
		}
		return &api.ChatResponse{Content: "done"}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) { c.Model = model }, read)
	if _, err := run(t, eng, "read big.go"); err != nil {
		t.Fatal(err)
	}
	for _, m := range eng.Messages() {
		if m.Role == "tool" {
			return m.Content
		}
	}
	t.Fatal("no tool result")
	return ""
}

// A single read was cut at ~6000 tokens (~18K characters) on every model, so a
// larger file took several reads. The limit now grows with the context window.
func TestToolOutputLimitGrowsWithTheContextWindow(t *testing.T) {
	if got := len(readTurnOutput(t, "deepseek-v4-pro", 200000)); got < 60000 {
		t.Fatalf("1M-window model kept only %d characters of a 200K-character read", got)
	}
	if got := len(readTurnOutput(t, "qwen-turbo", 200000)); got > 20000 {
		t.Fatalf("32K-window model kept %d characters; small windows keep the old limit", got)
	}
}
