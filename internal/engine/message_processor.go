package engine

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
)

// MessageNode is a lightweight graph node for conversation topology.
// The graph is linear today (parent = previous message), but the explicit
// structure allows smarter pruning/weighting policies without changing storage.
type MessageNode struct {
	ID         string
	ParentID   string
	Role       string
	Content    string
	Type       string
	Weight     float64
	TokenCount int
	Children   []string
}

// BuildMessageGraph materializes message history into weighted nodes.
func BuildMessageGraph(messages []api.Message) []MessageNode {
	nodes := make([]MessageNode, 0, len(messages))
	for i, m := range messages {
		n := MessageNode{
			ID:         makeNodeID(i),
			Role:       m.Role,
			Content:    m.Content,
			Type:       classifyMessageType(m),
			Weight:     messageWeight(m),
			TokenCount: len(m.Content)/4 + 1,
		}
		if i > 0 {
			n.ParentID = makeNodeID(i - 1)
		}
		nodes = append(nodes, n)
	}
	for i := range nodes {
		if i > 0 {
			nodes[i-1].Children = append(nodes[i-1].Children, nodes[i].ID)
		}
	}
	return nodes
}

func chooseCompressionSplitAssistant(messages []api.Message, keepCount int) int {
	splitIdx := len(messages) - keepCount
	if splitIdx <= 0 || splitIdx >= len(messages) {
		return -1
	}
	for splitIdx > 0 && splitIdx < len(messages) && messages[splitIdx].Role != "assistant" {
		splitIdx--
	}
	if splitIdx <= 0 {
		return -1
	}
	return splitIdx
}

func classifyMessageType(m api.Message) string {
	if strings.HasPrefix(strings.TrimSpace(m.Content), "<compress ") {
		return "compress"
	}
	switch m.Role {
	case "tool":
		return "tool_result"
	case "assistant":
		if len(m.ToolCalls) > 0 {
			return "tool_call"
		}
		return "assistant"
	case "user":
		return "user"
	default:
		return m.Role
	}
}

func messageWeight(m api.Message) float64 {
	w := 1.0
	switch m.Role {
	case "assistant":
		w = 1.4
	case "user":
		w = 1.6
	case "tool":
		w = 0.8
	}
	lower := strings.ToLower(m.Content)
	if strings.Contains(lower, "error") || strings.Contains(lower, "fail") {
		w += 0.6
	}
	for _, tc := range m.ToolCalls {
		if p, ok := tc.Input["filePath"].(string); ok && p != "" {
			if strings.TrimSpace(filepath.Base(p)) != "" {
				w += 0.5
			}
		}
	}
	return w
}

func makeNodeID(idx int) string {
	return "msg-" + strconv.Itoa(idx)
}
