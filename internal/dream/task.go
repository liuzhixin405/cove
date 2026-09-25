package dream

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// TaskStatus represents the current state of the dream task. (Status is the
// runner-level snapshot reported by Runner.Status.)
type TaskStatus string

const (
	StatusRunning   TaskStatus = "running"
	StatusCompleted TaskStatus = "completed"
	StatusFailed    TaskStatus = "failed"
)

// Turn represents a single assistant turn from the dream agent.
type Turn struct {
	Text         string
	ToolUseCount int
}

// Task holds the state of a running or completed dream task.
type Task struct {
	mu               sync.Mutex
	ID               string
	Status           TaskStatus
	SessionsReviewed int
	FilesTouched     []string
	Turns            []Turn
	StartTime        time.Time
	EndTime          time.Time
	PriorMtime       time.Time
	CancelFunc       func() // cancels the dream context
	// Usage is the run's token usage and cost so far (guarded by mu).
	Usage Usage
}

const maxTurns = 30

var (
	taskMu       sync.Mutex
	activeTasks  = make(map[string]*Task)
	lastFinished *Task // most recent completed/failed task, for reporting
)

// NewTask creates and registers a new dream task.
func NewTask(sessionsReviewed int, priorMtime time.Time, cancelFunc func()) *Task {
	taskMu.Lock()
	defer taskMu.Unlock()

	t := &Task{
		ID:               generateID(),
		Status:           StatusRunning,
		SessionsReviewed: sessionsReviewed,
		FilesTouched:     make([]string, 0),
		Turns:            make([]Turn, 0),
		StartTime:        time.Now(),
		PriorMtime:       priorMtime,
		CancelFunc:       cancelFunc,
	}
	activeTasks[t.ID] = t
	return t
}

// setUsage records the run's cumulative usage so far.
func (t *Task) setUsage(u Usage) {
	t.mu.Lock()
	t.Usage = u
	t.mu.Unlock()
}

// CurrentStatus returns the task's status.
//
// Status is written under t.mu by Complete/Fail, so it must be read under t.mu
// too. Reading the field directly — as ActiveTask and runDream's deferred
// cleanup both used to — meant the same field was effectively "protected" by
// two different mutexes, which protects nothing.
func (t *Task) CurrentStatus() TaskStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Status
}

// ActiveTask returns the currently running dream task, if any.
func ActiveTask() *Task {
	taskMu.Lock()
	candidates := make([]*Task, 0, len(activeTasks))
	for _, t := range activeTasks {
		candidates = append(candidates, t)
	}
	taskMu.Unlock()

	// t.mu is taken only after taskMu is released, keeping a single lock order
	// (taskMu before t.mu is never held simultaneously) so finishTask can
	// unregister without risking a deadlock against this function.
	for _, t := range candidates {
		if t.CurrentStatus() == StatusRunning {
			return t
		}
	}
	return nil
}

// AddTurn adds a turn to the dream task and updates file tracking.
func (t *Task) AddTurn(turn Turn, touchedPaths []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if turn.Text == "" && turn.ToolUseCount == 0 && len(touchedPaths) == 0 {
		return
	}

	seen := make(map[string]bool, len(t.FilesTouched))
	for _, p := range t.FilesTouched {
		seen[p] = true
	}

	var newPaths []string
	for _, p := range touchedPaths {
		if !seen[p] {
			seen[p] = true
			newPaths = append(newPaths, p)
		}
	}

	if len(newPaths) > 0 {
		t.FilesTouched = append(t.FilesTouched, newPaths...)
	}

	// Keep only most recent turns
	if len(t.Turns) >= maxTurns {
		t.Turns = t.Turns[1:]
	}
	t.Turns = append(t.Turns, turn)
}

// Complete marks the task as completed; see finish for the return value.
func (t *Task) Complete() bool { return t.finish(StatusCompleted) }

// Fail marks the task as failed; see finish for the return value.
func (t *Task) Fail() bool { return t.finish(StatusFailed) }

// finish records a terminal status and unregisters the task.
//
// Unregistering matters: activeTasks was only ever added to, so every
// consolidation run since process start stayed reachable (with its turns and
// touched-file lists) and ActiveTask had to scan them all. Keeping the last
// finished task is enough for the UI to report on the run that just ended.
//
// Only the first call counts: a task cancelled at exit (CancelActive) is
// failed there, and the run's goroutine finishing afterwards must not turn it
// into "completed". It reports whether this call finished the task.
func (t *Task) finish(status TaskStatus) bool {
	t.mu.Lock()
	if t.Status != StatusRunning {
		t.mu.Unlock()
		return false
	}
	t.Status = status
	t.EndTime = time.Now()
	t.CancelFunc = nil
	id := t.ID
	t.mu.Unlock()

	// t.mu is released first: taskMu is always acquired before t.mu elsewhere
	// (see ActiveTask), and taking them in the other order here would invert
	// the lock order.
	taskMu.Lock()
	lastFinished = t
	delete(activeTasks, id)
	taskMu.Unlock()
	return true
}

// LastFinishedTask returns the most recently completed or failed task, if any.
func LastFinishedTask() *Task {
	taskMu.Lock()
	defer taskMu.Unlock()
	return lastFinished
}

// idSeq disambiguates IDs generated within the same second. The timestamp alone
// has one-second resolution, so two runs starting in the same second produced
// the same ID and the second one overwrote the first in activeTasks.
var idSeq atomic.Uint64

// generateID creates a unique dream task ID.
func generateID() string {
	return fmt.Sprintf("dream-%s-%d", time.Now().Format("20060102-150405"), idSeq.Add(1))
}
