package session

import "sync"

// TaskRunner is a FIFO queue used by the interactive frontend worker.
// It lives in the session/core layer so UI implementations share one queue model.
type TaskRunner struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []string
	closed bool
}

func NewTaskRunner() *TaskRunner {
	q := &TaskRunner{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *TaskRunner) Enqueue(s string) {
	q.mu.Lock()
	q.items = append(q.items, s)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *TaskRunner) EnqueueFront(s string) {
	q.mu.Lock()
	q.items = append([]string{s}, q.items...)
	q.mu.Unlock()
	q.cond.Signal()
}

// Next blocks until an item is available. It returns the next item, a snapshot
// of queued items, and ok=false only when the queue is closed and drained.
func (q *TaskRunner) Next() (cur string, rest []string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return "", nil, false
	}
	cur = q.items[0]
	q.items = q.items[1:]
	rest = append([]string(nil), q.items...)
	return cur, rest, true
}

func (q *TaskRunner) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// Snapshot returns a copy of currently queued items (excluding in-flight task).
func (q *TaskRunner) Snapshot() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.items...)
}
