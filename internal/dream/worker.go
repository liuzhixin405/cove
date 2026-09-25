package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/fsatomic"
	"github.com/liuzhixin405/cove/internal/log"
)

// Values of LastRun.Result.
const (
	ResultRunning   = "running"
	ResultCompleted = "completed"
	ResultFailed    = "failed"
	ResultSkipped   = "skipped"
)

// WorkerFlag is the hidden command-line flag that puts cove in worker mode:
// `cove --dream-worker <sessions-dir>` runs one consolidation and exits.
const WorkerFlag = "--dream-worker"

// WorkerProjectFlag is the hidden flag, given before WorkerFlag, that names
// the project root whose memory directory the worker consolidates too.
const WorkerProjectFlag = "--dream-project"

// WorkerArgs is the worker command line (after any --profile): the project
// root when known, then WorkerFlag and the sessions directory.
func WorkerArgs(sessionsDir, projectRoot string) []string {
	var args []string
	if projectRoot != "" {
		args = append(args, WorkerProjectFlag, projectRoot)
	}
	return append(args, WorkerFlag, sessionsDir)
}

// maxLogBytes caps dream.log: past it, the log is moved to dream.log.1 when
// the next worker starts, so the file cannot grow without bound.
const maxLogBytes = 1 << 20

// LastRun is the record of the last session-end consolidation (a detached
// worker, or the inline fallback), kept in dream-last.json in the config
// directory so that /dream in a later process can report it.
type LastRun struct {
	Mode       string    `json:"mode"` // "worker" or "inline"
	PID        int       `json:"pid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
	// PriorConsolidatedAt is the lock's timestamp before this run took it,
	// so a run whose process died can be rolled back (recoverDeadWorker).
	PriorConsolidatedAt time.Time `json:"prior_consolidated_at,omitzero"`
	SessionsReviewed    int       `json:"sessions_reviewed"`
	FilesTouched        int       `json:"files_touched"`
	// InputTokens, OutputTokens and CostUSD are what the run's model calls
	// used and cost (priced like the session's billing).
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	// ProjectMemory is the project memory directory consolidated alongside
	// the global one ("" when none).
	ProjectMemory string `json:"project_memory,omitempty"`
}

func lastRunPath() string { return filepath.Join(configDir(), "dream-last.json") }

// LogPath is where a detached worker's output goes (dream.log in the config
// directory).
func LogPath() string { return filepath.Join(configDir(), "dream.log") }

// ReadLastRun returns the last session-end consolidation record; a zero
// LastRun (and nil) when there is none.
func ReadLastRun() (LastRun, error) {
	var lr LastRun
	data, err := os.ReadFile(lastRunPath())
	if err != nil {
		if os.IsNotExist(err) {
			return lr, nil
		}
		return lr, err
	}
	if err := json.Unmarshal(data, &lr); err != nil {
		return LastRun{}, err
	}
	return lr, nil
}

func writeLastRun(lr LastRun) error {
	data, err := json.MarshalIndent(lr, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	return fsatomic.WriteFile(lastRunPath(), data, 0o600)
}

// SessionEndDue reports whether the conversation that is ending should be
// consolidated: dream enabled, trigger session_end, at least MinTurns
// assistant turns, at least MinIntervalMinutes since the last consolidation
// (when set), and some session (the ending one included) touched since it.
// The string says why not, for the debug log.
func SessionEndDue(assistantTurns int, sessionsDir string) (bool, string) {
	cfg := LoadConfig()
	switch {
	case !cfg.Enabled:
		return false, "disabled"
	case cfg.Trigger != TriggerSessionEnd:
		return false, "trigger is " + cfg.Trigger
	case assistantTurns < cfg.MinTurns:
		return false, fmt.Sprintf("%d assistant turns, need %d", assistantTurns, cfg.MinTurns)
	}
	recoverDeadWorker()
	lastAt, err := ReadLastConsolidatedAt()
	if err != nil {
		return false, err.Error()
	}
	if cfg.MinIntervalMinutes > 0 && !lastAt.IsZero() {
		if since := time.Since(lastAt); since < time.Duration(cfg.MinIntervalMinutes)*time.Minute {
			return false, fmt.Sprintf("last consolidation %d min ago, min_interval_minutes is %d", int(since.Minutes()), cfg.MinIntervalMinutes)
		}
	}
	ids, err := ListSessionsTouchedSince(lastAt, sessionsDir)
	if err != nil {
		return false, err.Error()
	}
	if len(ids) == 0 {
		return false, "no session since the last consolidation"
	}
	return true, ""
}

// consolidateOnce runs one consolidation synchronously over every session
// touched since the last one (the session that just ended included) and
// records it in dream-last.json. A failed, cancelled or panicking run rolls
// the lock back. A run with nothing to review, or blocked by the lock, is
// skipped and returns nil; the "running" record is written only once the
// lock is held, and a skip never overwrites the record of a worker that is
// still running, so a second worker cannot hide the first one's run.
func (r *Runner) consolidateOnce(ctx context.Context, mode string) (out LastRun, runErr error) {
	lr := LastRun{Mode: mode, PID: os.Getpid(), StartedAt: time.Now(), Result: ResultRunning}
	finish := func(result string, err error) (LastRun, error) {
		lr.Result, lr.FinishedAt = result, time.Now()
		if err != nil {
			lr.Error = err.Error()
		}
		if result == ResultSkipped && liveRunningRecord() {
			log.Infof("[dream] %s run skipped: %s", mode, lr.Error)
			return lr, nil
		}
		if werr := writeLastRun(lr); werr != nil {
			log.Warnf("[dream] write %s: %v", lastRunPath(), werr)
		}
		if result == ResultFailed {
			return lr, err
		}
		return lr, nil
	}

	recoverDeadWorker()
	lastAt, err := ReadLastConsolidatedAt()
	if err != nil {
		return finish(ResultFailed, err)
	}
	// Listed before the lock is taken: taking it stamps the lock, and
	// "touched since the last consolidation" is then nothing.
	ids, err := ListSessionsTouchedSince(lastAt, r.sessionsDir)
	if err != nil {
		return finish(ResultFailed, err)
	}
	if len(ids) == 0 {
		return finish(ResultSkipped, errors.New("没有新会话需要整理"))
	}
	priorMtime, acquired, err := TryAcquireConsolidationLock()
	if err != nil {
		return finish(ResultFailed, err)
	}
	if !acquired {
		return finish(ResultSkipped, errors.New("整理锁被另一个进程占用"))
	}
	lr.StartedAt, lr.PriorConsolidatedAt = time.Now(), priorMtime
	if err := writeLastRun(lr); err != nil {
		log.Warnf("[dream] write %s: %v", lastRunPath(), err)
	}
	var task *Task
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		// runDream's own deferred Fail has usually rolled the lock back
		// already; Fail reports false then and the lock is left alone.
		if task == nil || task.Fail() {
			_ = RollbackConsolidationLock(priorMtime)
		}
		if task != nil {
			// What the run had spent before it blew up is still spent.
			task.mu.Lock()
			u := task.Usage
			task.mu.Unlock()
			lr.InputTokens, lr.OutputTokens, lr.CostUSD = u.InputTokens, u.OutputTokens, u.CostUSD
		}
		log.Errorf("[dream] %s run panicked: %v", mode, p)
		out, runErr = finish(ResultFailed, fmt.Errorf("dream run panic: %v", p))
	}()
	if r.provider == nil {
		_ = RollbackConsolidationLock(priorMtime)
		return finish(ResultFailed, errors.New("dream: no model provider"))
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	task = NewTask(len(ids), priorMtime, cancel)
	log.Infof("[dream] %s run — %d sessions to review", mode, len(ids))
	if roots := r.memoryRoots(); len(roots) > 1 {
		lr.ProjectMemory = roots[1]
	}
	err = r.runDream(runCtx, task, ids)
	task.mu.Lock()
	files := len(task.FilesTouched)
	usage := task.Usage
	task.mu.Unlock()
	r.mu.Lock()
	r.lastRunFiles, r.lastRunErr, r.lastRunAt, r.lastRunUsage = files, err, time.Now(), usage
	r.mu.Unlock()
	lr.SessionsReviewed, lr.FilesTouched = len(ids), files
	lr.InputTokens, lr.OutputTokens, lr.CostUSD = usage.InputTokens, usage.OutputTokens, usage.CostUSD
	if err != nil {
		return finish(ResultFailed, err)
	}
	return finish(ResultCompleted, nil)
}

// liveRunningRecord reports whether dream-last.json describes a run that
// another, still living process is doing.
func liveRunningRecord() bool {
	lr, err := ReadLastRun()
	return err == nil && lr.Result == ResultRunning && lr.PID != os.Getpid() && isProcessRunning(lr.PID)
}

// deadWorkerLockSlack is how far the lock's timestamp may be from a dead
// run's StartedAt for the lock to count as that run's: the record is written
// right after the lock is stamped.
const deadWorkerLockSlack = time.Minute

// recoverDeadWorker handles a worker that was killed outright (or crashed
// past every defer): dream-last.json still says running, its PID is gone,
// and the lock carries the timestamp that run stamped — so its sessions
// would count as consolidated. The lock is rolled back to what it was before
// that run and the record marked failed. A lock someone stamped since is left
// alone.
func recoverDeadWorker() {
	lr, err := ReadLastRun()
	if err != nil || lr.Result != ResultRunning || lr.PID == os.Getpid() || isProcessRunning(lr.PID) {
		return
	}
	lockAt, err := ReadLastConsolidatedAt()
	if err == nil && !lockAt.IsZero() {
		if d := lockAt.Sub(lr.StartedAt); d > -deadWorkerLockSlack && d < deadWorkerLockSlack {
			if err := RollbackConsolidationLock(lr.PriorConsolidatedAt); err != nil && !os.IsNotExist(err) {
				log.Warnf("[dream] roll back the dead worker's lock: %v", err)
				return
			}
			log.Warnf("[dream] worker PID %d died mid-run; consolidation lock rolled back", lr.PID)
		}
	}
	lr.Result, lr.FinishedAt = ResultFailed, time.Now()
	lr.Error = fmt.Sprintf("整理进程 PID %d 异常退出（被终止或崩溃），整理锁已回滚", lr.PID)
	if err := writeLastRun(lr); err != nil {
		log.Warnf("[dream] write %s: %v", lastRunPath(), err)
	}
}

// RunWorker is `cove --dream-worker`: one consolidation of the sessions in
// sessionsDir with the given provider and model, bounded by dreamRunTimeout,
// over the global memory directory and, when projectRoot is set, that
// project's memory directory (config.ProjectDataDir(projectRoot)/memory),
// recorded in dream-last.json. A disabled dream is recorded as skipped; a
// provider that fails Validate (no API key) fails the run before the lock is
// taken.
func RunWorker(ctx context.Context, p api.Provider, model, sessionsDir, projectRoot string) error {
	if !LoadConfig().Enabled {
		_ = writeLastRun(LastRun{Mode: "worker", PID: os.Getpid(), StartedAt: time.Now(), FinishedAt: time.Now(), Result: ResultSkipped, Error: "dream 已禁用"})
		return nil
	}
	if p == nil {
		return errors.New("dream worker: no provider")
	}
	if err := p.Validate(); err != nil {
		err = fmt.Errorf("模型配置不可用: %w", err)
		now := time.Now()
		_ = writeLastRun(LastRun{Mode: "worker", PID: os.Getpid(), StartedAt: now, FinishedAt: now, Result: ResultFailed, Error: err.Error()})
		return err
	}
	r := &Runner{provider: p, model: model, memoryRoot: memoryDir(), sessionsDir: sessionsDir}
	r.SetProjectRoot(projectRoot)
	ctx, cancel := context.WithTimeout(ctx, dreamRunTimeout)
	defer cancel()
	_, err := r.consolidateOnce(ctx, "worker")
	return err
}

// inlineGrace is how long RunInline waits, after its budget ran out, for the
// run to notice the cancellation before it gives up on it.
const inlineGrace = 2 * time.Second

// RunInline is the fallback when the worker process cannot be started: one
// consolidation in this process, cancelled (and its lock rolled back) once
// budget runs out. tick is called every tickEvery while it runs (progress
// dots on the terminal).
func (r *Runner) RunInline(ctx context.Context, budget, tickEvery time.Duration, tick func()) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.consolidateOnce(ctx, "inline")
		done <- err
	}()
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if tick != nil {
				tick()
			}
		case <-ctx.Done():
			select {
			case err := <-done:
				return err
			case <-time.After(inlineGrace):
				// The model call ignores its context: fail the task and roll
				// the lock back here, since the process is about to exit.
				CancelActive()
				return fmt.Errorf("dream: inline run exceeded %v", budget)
			}
		}
	}
}

// SpawnWorker starts `cove <args...>` (os.Executable) as a detached process —
// its own process group / session, no console — with stdout and stderr
// appended to dream.log, and returns its PID without waiting for it.
func SpawnWorker(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	logPath := LogPath()
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogBytes {
		_ = os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logf.Close() }() // the child has its own handle
	_, _ = fmt.Fprintf(logf, "%s starting %s %v\n", time.Now().Format(time.RFC3339), filepath.Base(exe), args)
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	// The working directory is inherited on purpose: the worker loads the
	// config the way the parent did, project .cove.json (model, provider)
	// included.
	cmd.SysProcAttr = detachedAttr()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}
