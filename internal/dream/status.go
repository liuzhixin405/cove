package dream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

// ErrDisabled is returned by RunNow when consolidation is turned off in
// ~/.cove/dream.json.
var ErrDisabled = errors.New("dream is disabled")

// ErrLockHeld is returned by RunNow when another run (this process or a live
// other one) holds the consolidation lock.
var ErrLockHeld = errors.New("another consolidation holds the lock")

// Status is a snapshot of the auto-dream state, for /dream, /doctor and the
// turn-end summary.
type Status struct {
	Enabled bool
	// Trigger is the dream.json trigger mode (TriggerSessionEnd or
	// TriggerThreshold); "" is treated as threshold (a Status built by hand).
	Trigger string
	// MinTurns is the session_end gate: assistant turns a session needs.
	MinTurns           int
	LastConsolidatedAt time.Time // zero when there has never been a consolidation
	// HoursSinceLast is the time since LastConsolidatedAt in hours, or -1 when
	// there has never been one (the time gate is then open).
	HoursSinceLast float64
	// SessionsSinceLast counts sessions touched since LastConsolidatedAt,
	// excluding the runner's current session — exactly what the session gate
	// compares against MinSessions.
	SessionsSinceLast int
	MinHours          int
	MinSessions       int
	// Running reports a consolidation in progress (any runner in this process).
	Running bool
	// LastRunFilesTouched, LastRunErr and LastRunAt describe this runner's
	// last finished run: files it wrote, why it failed (nil when it completed
	// or none ran) and when it ended (zero when none ran).
	LastRunFilesTouched int
	LastRunErr          error
	LastRunAt           time.Time
	// Suppressed is non-empty when automatic runs are switched off for this
	// process (SuppressAuto), e.g. under -p.
	Suppressed string
	// LastWorkerStartedAt and LastWorkerResult describe the last session-end
	// consolidation (dream-last.json, any process): when it started (zero
	// when none ran) and a one-line Chinese account of how it went.
	LastWorkerStartedAt time.Time
	LastWorkerResult    string
	// LastRunInputTokens, LastRunOutputTokens and LastRunCostUSD are the
	// usage of the most recent consolidation: this runner's last run when it
	// had one, else the session-end record (dream-last.json).
	LastRunInputTokens  int
	LastRunOutputTokens int
	LastRunCostUSD      float64
}

func (s Status) sessionEnd() bool { return s.Trigger == TriggerSessionEnd }

// SessionsNeeded is how many more sessions the session gate is waiting for
// (0 when it is already open).
func (s Status) SessionsNeeded() int {
	if s.sessionEnd() {
		return 0 // no session gate: the conversation's end triggers the run
	}
	return max(s.MinSessions-s.SessionsSinceLast, 0)
}

// HoursNeeded is how many more hours the time gate is waiting for (0 when it
// is already open).
func (s Status) HoursNeeded() float64 {
	if s.sessionEnd() || s.HoursSinceLast < 0 {
		return 0
	}
	return max(float64(s.MinHours)-s.HoursSinceLast, 0)
}

// Summary is a one-line Chinese description: enabled or not, time since the
// last consolidation, and what the gates still wait for (or that it runs).
func (s Status) Summary() string {
	if !s.Enabled {
		return "已禁用 (~/.cove/dream.json)"
	}
	var parts []string
	if s.LastConsolidatedAt.IsZero() {
		parts = append(parts, "从未整理")
	} else {
		parts = append(parts, fmt.Sprintf("距上次整理 %.1f 小时", s.HoursSinceLast))
	}
	switch {
	case s.Running:
		parts = append(parts, "正在整理")
	case s.sessionEnd():
		parts = append(parts, fmt.Sprintf("对话结束时整理（至少 %d 个回合）", s.MinTurns))
	case s.Suppressed != "":
		parts = append(parts, "本进程不自动整理")
	default:
		h, n := s.HoursNeeded(), s.SessionsNeeded()
		if h > 0 {
			parts = append(parts, fmt.Sprintf("还差 %.1f 小时", h))
		}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("还差 %d 个会话", n))
		}
		if h == 0 && n == 0 {
			parts = append(parts, "门槛已满足，下个回合结束时整理")
		}
	}
	return strings.Join(parts, "，")
}

// Status reports the runner's gates and its last run. It reads the lock file
// and stats the session files, so it is cheap but not free.
func (r *Runner) Status() Status {
	cfg := LoadConfig()
	st := Status{
		Enabled:        cfg.Enabled,
		Trigger:        cfg.Trigger,
		MinTurns:       cfg.MinTurns,
		MinHours:       cfg.MinHours,
		MinSessions:    cfg.MinSessions,
		HoursSinceLast: -1,
		Running:        ActiveTask() != nil,
		Suppressed:     autoSuppressed(),
	}
	if lastAt, err := ReadLastConsolidatedAt(); err == nil && !lastAt.IsZero() {
		st.LastConsolidatedAt = lastAt
		st.HoursSinceLast = time.Since(lastAt).Hours()
	}
	if ids, err := r.sessionsSince(st.LastConsolidatedAt); err == nil {
		st.SessionsSinceLast = len(ids)
	}
	var diskAt time.Time
	if lr, err := ReadLastRun(); err == nil && !lr.StartedAt.IsZero() {
		st.LastWorkerStartedAt = lr.StartedAt
		st.LastWorkerResult = describeLastRun(lr)
		st.LastRunInputTokens, st.LastRunOutputTokens, st.LastRunCostUSD = lr.InputTokens, lr.OutputTokens, lr.CostUSD
		diskAt = lr.FinishedAt
	}
	r.mu.Lock()
	st.LastRunFilesTouched = r.lastRunFiles
	st.LastRunErr = r.lastRunErr
	st.LastRunAt = r.lastRunAt
	if !r.lastRunAt.IsZero() && r.lastRunAt.After(diskAt) {
		u := r.lastRunUsage
		st.LastRunInputTokens, st.LastRunOutputTokens, st.LastRunCostUSD = u.InputTokens, u.OutputTokens, u.CostUSD
	}
	r.mu.Unlock()
	return st
}

// StatusFromDisk is Status for a process that has no runner (cove doctor):
// the gates and the last consolidation time, with no current session and no
// last-run details.
func StatusFromDisk() Status {
	home := homeDir()
	return (&Runner{sessionsDir: sessionsDirIn(home)}).Status()
}

// SetCurrentSession changes the session the session gate excludes. The
// engine calls it when it switches to another session (resume, /clear): the
// ID given to NewRunner is otherwise stale and the session in use would count
// as one to consolidate.
func (r *Runner) SetCurrentSession(id string) {
	r.mu.Lock()
	r.currentSession = id
	r.mu.Unlock()
}

// SessionsDir is the directory whose sessions this runner consolidates (the
// argument a session-end worker is started with).
func (r *Runner) SessionsDir() string { return r.sessionsDir }

// DefaultSessionsDir is ~/.cove/sessions, for an exit path with no runner.
func DefaultSessionsDir() string { return sessionsDirIn(homeDir()) }

// sessionsSince lists sessions touched after since, minus the current one.
func (r *Runner) sessionsSince(since time.Time) ([]string, error) {
	ids, err := ListSessionsTouchedSince(since, r.sessionsDir)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	current := r.currentSession
	r.mu.Unlock()
	filtered := ids[:0]
	for _, id := range ids {
		if id != current {
			filtered = append(filtered, id)
		}
	}
	return filtered, nil
}

// RunNow starts a consolidation immediately, ignoring the time and session
// gates (the /dream run command). It still needs consolidation enabled and the
// lock; the run itself proceeds in the background like an automatic one, and
// RunNow returns once it has started. It returns the number of sessions the
// run reviews — counted before the lock is taken, since taking it stamps the
// lock and "sessions since the last consolidation" is then zero.
func (r *Runner) RunNow(ctx context.Context) (int, error) {
	if !IsEnabled() {
		return 0, ErrDisabled
	}
	lastAt, err := ReadLastConsolidatedAt()
	if err != nil {
		return 0, err
	}
	sessionIDs, err := r.sessionsSince(lastAt)
	if err != nil {
		return 0, err
	}
	n := len(sessionIDs)
	priorMtime, acquired, err := TryAcquireConsolidationLock()
	if err != nil {
		return n, err
	}
	if !acquired {
		return n, ErrLockHeld
	}
	if r.provider == nil {
		_ = RollbackConsolidationLock(priorMtime)
		return n, fmt.Errorf("dream: no model provider")
	}
	log.Debugf("[autoDream] manual run — %d sessions to review", n)
	r.start(ctx, priorMtime, sessionIDs)
	return n, nil
}

// CancelActive stops the consolidation running in this process, if any, and
// rolls the lock's timestamp back, reporting whether there was one. Exit paths
// call it: the process is about to end, and a run killed midway would
// otherwise leave the lock stamped as though it had completed.
func CancelActive() bool {
	t := ActiveTask()
	if t == nil {
		return false
	}
	t.mu.Lock()
	cancel := t.CancelFunc
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !t.Fail() {
		return false // it finished on its own meanwhile
	}
	if err := RollbackConsolidationLock(t.PriorMtime); err != nil {
		log.Debugf("[autoDream] rollback at exit: %v", err)
	}
	log.Debugf("[autoDream] cancelled at exit; lock rolled back")
	return true
}

// Current returns the runner most recently created by NewRunner (the
// engine's), or nil. Slash commands and diagnostics reach the runner through
// it without the engine having to hand it over.
func Current() *Runner {
	currentMu.Lock()
	defer currentMu.Unlock()
	return currentRunner
}

var (
	currentMu     sync.Mutex
	currentRunner *Runner
	suppressed    string
)

func setCurrent(r *Runner) {
	currentMu.Lock()
	currentRunner = r
	currentMu.Unlock()
}

// SuppressAuto turns automatic consolidation off for this process; reason is
// logged when a run is skipped. An empty reason turns it back on. cove -p
// uses it: the process exits right after the answer, which would kill a run
// midway with the lock already stamped as if it had finished.
func SuppressAuto(reason string) {
	currentMu.Lock()
	suppressed = reason
	currentMu.Unlock()
}

func autoSuppressed() string {
	currentMu.Lock()
	defer currentMu.Unlock()
	return suppressed
}

// describeLastRun is the one-line Chinese account of a session-end run.
func describeLastRun(lr LastRun) string {
	where := "后台"
	if lr.Mode == "inline" {
		where = "前台"
	}
	switch lr.Result {
	case ResultRunning:
		if isProcessRunning(lr.PID) {
			return fmt.Sprintf("%s正在整理（PID %d，开始于 %s）", where, lr.PID, lr.StartedAt.Format("01-02 15:04"))
		}
		return fmt.Sprintf("%s整理异常退出（开始于 %s，未记录结果，详见 %s）", where, lr.StartedAt.Format("01-02 15:04"), LogPath())
	case ResultCompleted:
		return fmt.Sprintf("%s整理完成：回顾 %d 个会话，写入 %d 个文件（%s 结束）", where, lr.SessionsReviewed, lr.FilesTouched, lr.FinishedAt.Format("01-02 15:04"))
	case ResultSkipped:
		return fmt.Sprintf("%s整理跳过：%s（%s）", where, lr.Error, lr.FinishedAt.Format("01-02 15:04"))
	default:
		return fmt.Sprintf("%s整理失败：%s（%s，详见 %s）", where, lr.Error, lr.FinishedAt.Format("01-02 15:04"), LogPath())
	}
}
