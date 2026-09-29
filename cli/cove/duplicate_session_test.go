package main

import (
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// Re-sending a request that an unfinished session of this project already
// started continues that session instead of opening a fifth copy of it:
// /history showed the same task four times after four restarts.
func TestFindDuplicateSessionPicksTheUnfinishedTwin(t *testing.T) {
	req := "D:\\github\\agent 在该目录写一个netcore的agent框架的项目，使用ai本地模型"
	finished := session.Record{ID: "a", UpdatedAt: time.Now().Add(-3 * time.Hour), Messages: []api.Message{
		{Role: "user", Content: req},
		{Role: "assistant", Content: "项目已创建完成。"},
	}}
	interrupted := session.Record{ID: "b", UpdatedAt: time.Now().Add(-time.Hour), Messages: []api.Message{
		{Role: "user", Content: req},
		{Role: "assistant", Content: "我来看看环境", ToolCalls: []api.ToolCall{{ID: "1", Name: "bash"}}},
		{Role: "tool", ToolCallID: "1", Content: "ok"},
	}}
	other := session.Record{ID: "c", UpdatedAt: time.Now(), Messages: []api.Message{
		{Role: "user", Content: "hi"},
	}}
	records := []session.Record{other, interrupted, finished}

	got, idx := findDuplicateSession(records, req)
	if got == nil || got.ID != "b" || idx != 2 {
		t.Fatalf("got %v idx %d, want the interrupted twin at #2", got, idx)
	}
	// Punctuation and spacing differences are the same request.
	if got, _ := findDuplicateSession(records, "D:\\github\\agent 在该目录写一个netcore的agent框架的项目 使用ai本地模型。"); got == nil {
		t.Error("a cosmetically different re-send was not matched")
	}
	if got, _ := findDuplicateSession(records, "写一个别的东西"); got != nil {
		t.Errorf("unrelated request matched %v", got)
	}
	if got, _ := findDuplicateSession(records, "hi"); got != nil {
		t.Errorf("a trivial request matched a session: %v", got)
	}
	// A finished session is not continued: the person wants the task again.
	if got, _ := findDuplicateSession([]session.Record{finished}, req); got != nil {
		t.Errorf("finished session matched: %v", got)
	}
}

func TestSessionUnfinished(t *testing.T) {
	if sessionUnfinished(session.Record{Messages: []api.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "done"}}}) {
		t.Error("a session ending in an answer counts as unfinished")
	}
	if !sessionUnfinished(session.Record{Messages: []api.Message{{Role: "user", Content: "q"}}}) {
		t.Error("a session with only the request counts as finished")
	}
	if !sessionUnfinished(session.Record{Messages: []api.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "x"},
		{Role: "user", Content: "[system: The previous turn was interrupted (模型调用失败). Commands may have partially executed…]"}}}) {
		t.Error("an interrupted session counts as finished")
	}
}
