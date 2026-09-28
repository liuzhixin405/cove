package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// truncateDesc cut at a byte offset and split Chinese characters in /help.
func TestTruncateDescKeepsRunesWhole(t *testing.T) {
	got := truncateDesc("列出当前项目的所有检查点并显示时间", 8)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateDesc produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 8 {
		t.Fatalf("truncateDesc kept %d runes, want at most 8: %q", n, got)
	}
}

// /new in the real REPL: the next request carries none of the old
// conversation.
func TestE2E_NewStartsAnEmptyConversation(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{Content: "第一件事已经处理完了，结果在上面。"},
		fakeStep{Content: "第二件事也处理完了，和前面的对话无关。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("第一件事：旧对话里的暗号 zebra-42")
	s.WaitFor("第一件事已经处理完了", e2eTimeout)
	// The answer is printed before the task has finished (session save,
	// turn-end work); /new is refused until then, as it should be.
	deadline := time.Now().Add(e2eTimeout)
	for !strings.Contains(s.Output(), "[新会话]") {
		if time.Now().After(deadline) {
			t.Fatalf("/new never accepted:\n%s", s.Output())
		}
		s.Type("/new")
		time.Sleep(300 * time.Millisecond)
	}
	s.Type("第二件事")
	s.WaitFor("第二件事也处理完了", e2eTimeout)

	reqs := model.Requests()
	if len(reqs) < 2 {
		t.Fatalf("model got %d requests, want 2", len(reqs))
	}
	for _, m := range reqs[len(reqs)-1].Messages {
		if strings.Contains(m.Text(), "zebra-42") {
			t.Fatalf("the request after /new still carries the old conversation: %q", m.Text())
		}
	}
}
