package dream

import (
	"sync"
	"time"
)

// Phase represents the current stage of the dream task.
type Phase string

const (
	PhaseStarting Phase = "starting"
	PhaseUpdating Phase = "updating"
)

// Status represents the current state of the dream task.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusKilled    Status = "killed"
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
	Status           Status
	Phase            Phase
	SessionsReviewed int
	FilesTouched     []string
	Turns            []Turn
	StartTime        time.Time
	EndTime          time.Time
	PriorMtime       time.Time
	CancelFunc       func() // cancels the dream context
}

const maxTurns = 30

var (
	taskMu      sync.Mutex
	activeTasks = make(map[string]*Task)
)

// NewTask creates and registers a new dream task.
func NewTask(sessionsReviewed int, priorMtime time.Time, cancelFunc func()) *Task {
	taskMu.Lock()
	defer taskMu.Unlock()

	t := &Task{
		ID:               generateID(),
		Status:           StatusRunning,
		Phase:            PhaseStarting,
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

// GetTask returns a dream task by ID.
func GetTask(id string) *Task {
	taskMu.Lock()
	defer taskMu.Unlock()
	return activeTasks[id]
}

// ActiveTask returns the currently running dream task, if any.
func ActiveTask() *Task {
	taskMu.Lock()
	defer taskMu.Unlock()
	for _, t := range activeTasks {
		if t.Status == StatusRunning {
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
		t.Phase = PhaseUpdating
		t.FilesTouched = append(t.FilesTouched, newPaths...)
	}

	// Keep only most recent turns
	if len(t.Turns) >= maxTurns {
		t.Turns = t.Turns[1:]
	}
	t.Turns = append(t.Turns, turn)
}

// Complete marks the task as completed.
func (t *Task) Complete() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = StatusCompleted
	t.EndTime = time.Now()
	t.CancelFunc = nil
}

// Fail marks the task as failed.
func (t *Task) Fail() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = StatusFailed
	t.EndTime = time.Now()
	t.CancelFunc = nil
}

// Kill aborts the dream and rolls back the lock.
func (t *Task) Kill() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Status != StatusRunning {
		return
	}
	if t.CancelFunc != nil {
		t.CancelFunc()
	}
	t.Status = StatusKilled
	t.EndTime = time.Now()
	t.CancelFunc = nil

	// Rollback lock asynchronously
	go RollbackConsolidationLock(t.PriorMtime)
}

// generateID creates a unique dream task ID.
func generateID() string {
	return "dream-" + time.Now().Format("20060102-150405")
}
