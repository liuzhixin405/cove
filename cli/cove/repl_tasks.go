package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/repl"
)

type replTaskRunner struct {
	mu               sync.Mutex
	cond             *sync.Cond
	eng              *engine.Engine
	running          bool
	cancel           context.CancelFunc
	queue            []api.Message
	pendingFailedMsg *api.Message
}

func isContinueCommand(input string) bool {
	v := strings.TrimSpace(strings.ToLower(input))
	if v == "继续" || v == "continue" {
		return true
	}
	return strings.HasPrefix(v, "继续") || strings.HasPrefix(v, "continue ")
}

func canMergeQueuedTask(existing, incoming api.Message) bool {
	if existing.Role != "user" || incoming.Role != "user" {
		return false
	}
	if len(existing.Parts) > 0 || len(incoming.Parts) > 0 {
		return false
	}
	a := normalizeTaskForMerge(existing.Content)
	b := normalizeTaskForMerge(incoming.Content)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	if commonPrefixRunes(a, b) >= 24 {
		return true
	}
	return false
}

func mergeQueuedTask(existing, incoming api.Message) api.Message {
	merged := existing
	add := strings.TrimSpace(incoming.Content)
	if add == "" {
		return merged
	}
	base := strings.TrimSpace(existing.Content)
	if base == "" {
		merged.Content = add
		return merged
	}
	if strings.Contains(base, add) {
		return merged
	}
	merged.Content = base + "\n\n[补充要求]\n" + add
	return merged
}

func normalizeTaskForMerge(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"\n", " ", "\t", " ",
		"，", " ", "。", " ", "！", " ", "？", " ",
		",", " ", ".", " ", "!", " ", "?", " ",
		"：", " ", ":", " ", ";", " ", "；", " ",
	)
	v = replacer.Replace(v)
	return strings.Join(strings.Fields(v), " ")
}

func commonPrefixRunes(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	n := len(ar)
	if len(br) < n {
		n = len(br)
	}
	count := 0
	for i := 0; i < n; i++ {
		if ar[i] != br[i] {
			break
		}
		count++
	}
	return count
}

func newREPLTaskRunner(eng *engine.Engine) *replTaskRunner {
	r := &replTaskRunner{
		eng:   eng,
		queue: make([]api.Message, 0),
	}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *replTaskRunner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *replTaskRunner) CancelRunning() bool {
	r.mu.Lock()
	cancel := r.cancel
	running := r.running
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return running
}

func (r *replTaskRunner) PendingFailed() *api.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingFailedMsg
}

func (r *replTaskRunner) ClearPendingFailed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingFailedMsg = nil
}

func (r *replTaskRunner) Enqueue(msg api.Message) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running && len(r.queue) > 0 {
		for i := len(r.queue) - 1; i >= 0; i-- {
			if canMergeQueuedTask(r.queue[i], msg) {
				r.queue[i] = mergeQueuedTask(r.queue[i], msg)
				return i, true
			}
		}
	}

	r.queue = append(r.queue, msg)
	queueSize := len(r.queue)
	r.startNextLocked()
	if r.running {
		if queueSize > 0 {
			return queueSize - 1, false
		}
		return 0, false
	}
	return 0, false
}

func (r *replTaskRunner) WaitIdle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.running {
		r.cond.Wait()
	}
}

func (r *replTaskRunner) WaitIdleUntil(deadline time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.running {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.AfterFunc(remaining, func() {
			r.mu.Lock()
			r.cond.Broadcast()
			r.mu.Unlock()
		})
		r.cond.Wait()
		timer.Stop()
	}
	return true
}

func (r *replTaskRunner) startNextLocked() {
	if r.running || len(r.queue) == 0 {
		return
	}
	msg := r.queue[0]
	r.queue = r.queue[1:]
	r.running = true
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	go r.run(ctx, msg)
}

func (r *replTaskRunner) run(ctx context.Context, userMsg api.Message) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Warnf("repl task panic: %v", recovered)
			r.mu.Lock()
			msgCopy := userMsg
			r.pendingFailedMsg = &msgCopy
			_ = saveInterruptedDraft(userMsg, fmt.Errorf("internal panic: %v", recovered))
			r.finishLocked()
			repl.PrintAbove(fmt.Sprintf("\r\n%s任务执行出现内部异常，已恢复输入。可输入“继续”重试。%s\r\n", repl.Red, repl.Reset))
			r.startNextLocked()
			r.mu.Unlock()
		}
	}()

	_, reqErr := runChatInteractionMessage(ctx, r.eng, userMsg)

	r.mu.Lock()
	defer r.mu.Unlock()

	if reqErr != nil {
		msgCopy := userMsg
		r.pendingFailedMsg = &msgCopy
		if isBudgetExceededError(reqErr) {
			repl.PrintAbove(budgetExceededRetryHint(r.eng.CostTracker()) + "\n")
		} else {
			_ = saveInterruptedDraft(userMsg, reqErr)
			repl.PrintAbove("可输入“继续”重试刚才中断的任务。\n")
		}
	} else {
		r.pendingFailedMsg = nil
		_ = clearInterruptedDraft()
	}

	r.finishLocked()
	r.startNextLocked()
}

func (r *replTaskRunner) finishLocked() {
	r.running = false
	r.cancel = nil
	r.cond.Broadcast()
}
