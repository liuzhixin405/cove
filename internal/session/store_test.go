package session

import "testing"

func TestListMetadataCountsUserTurnsAndToolMessages(t *testing.T) {
	messages := []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{
		{Role: "user", Content: "请修改代码"},
		{Role: "assistant", Content: "我来处理"},
		{Role: "tool", Content: "read result"},
		{Role: "tool", Content: "test output"},
		{Role: "user", Content: "[system: retry differently]"},
		{Role: "user", Content: "继续"},
	}

	if got := countGenuineUserTurns(messages); got != 2 {
		t.Fatalf("countGenuineUserTurns = %d, want 2", got)
	}
	if got := countToolMessages(messages); got != 2 {
		t.Fatalf("countToolMessages = %d, want 2", got)
	}
}
