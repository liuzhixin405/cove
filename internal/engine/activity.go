package engine

import (
	"fmt"
	"time"

	"github.com/liuzhixin405/cove/internal/diagnostic"
)

// stallThreshold is how long a single stage may go without making progress
// before the engine surfaces a "可能卡住" hint. This turns an opaque hang at
// "思考中..." into an attributable stage so the user knows where it is stuck.
const stallThreshold = 30 * time.Second

// activity is one in-flight blocking stage tracked by the stall monitor.
type activity struct {
	label        string
	start        time.Time
	lastProgress time.Time
	paused       bool          // true while legitimately waiting on the user
	lastNotified time.Duration // idle value at last stall notification (for escalation)
}

// beginActivity registers a new in-flight stage and returns its id.
func (e *Engine) beginActivity(label string) uint64 {
	e.actMu.Lock()
	defer e.actMu.Unlock()
	if e.acts == nil {
		e.acts = make(map[uint64]*activity)
	}
	e.actSeq++
	id := e.actSeq
	now := time.Now()
	e.acts[id] = &activity{label: label, start: now, lastProgress: now}
	return id
}

// progressActivity records that the given stage just made progress, resetting
// its stall timer (e.g. a streaming delta arrived).
func (e *Engine) progressActivity(id uint64) {
	e.actMu.Lock()
	if a := e.acts[id]; a != nil {
		a.lastProgress = time.Now()
		a.lastNotified = 0
	}
	e.actMu.Unlock()
}

// pauseActivity marks a stage as legitimately waiting (e.g. on a user
// permission prompt) so the monitor does not falsely flag it as stuck.
func (e *Engine) pauseActivity(id uint64, paused bool) {
	e.actMu.Lock()
	if a := e.acts[id]; a != nil {
		a.paused = paused
		if !paused {
			a.lastProgress = time.Now()
			a.lastNotified = 0
		}
	}
	e.actMu.Unlock()
}

// endActivity removes a completed stage.
func (e *Engine) endActivity(id uint64) {
	e.actMu.Lock()
	delete(e.acts, id)
	e.actMu.Unlock()
}

// runStallMonitor periodically scans in-flight activities and, if any has made
// no progress for stallThreshold, prints a diagnostic line naming the stuck
// stage and records it for later inspection. Stop the monitor by closing stop.
func (e *Engine) runStallMonitor(stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			now := time.Now()
			type stuck struct {
				label string
				idle  time.Duration
			}
			var stuckList []stuck
			e.actMu.Lock()
			for _, a := range e.acts {
				if a.paused {
					continue
				}
				idle := now.Sub(a.lastProgress)
				// Notify once at the threshold, then re-notify every further
				// threshold so the user sees the elapsed time keep growing.
				if idle >= stallThreshold && idle-a.lastNotified >= stallThreshold {
					a.lastNotified = idle
					stuckList = append(stuckList, stuck{a.label, idle})
				}
			}
			e.actMu.Unlock()
			for _, s := range stuckList {
				e.engineOutput(fmt.Sprintf(
					"\r\x1b[K\x1b[33m! still in '%s', no progress for %s (possibly stuck, press Ctrl+C to interrupt)\x1b[0m\n",
					s.label, s.idle.Round(time.Second)))
				// Record once to the runtime log. We deliberately do NOT also call
				// log.Warnf here: the live stderr line above already shows it, and
				// log.Warnf would be mirrored into the same log via the sink,
				// producing a duplicate entry.
				diagnostic.RecordRuntime(diagnostic.SevWarning, diagnostic.CatEngine,
					fmt.Sprintf("stage '%s' stalled with no progress for %s", s.label, s.idle.Round(time.Second)))
			}
		}
	}
}
