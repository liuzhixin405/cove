package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
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
				first bool // first notification for this stall
			}
			var stuckList []stuck
			e.actMu.Lock()
			for _, a := range e.acts {
				if a.paused {
					continue
				}
				idle := now.Sub(a.lastProgress)
				threshold := e.stallThresholdFor(a.label)
				// Notify once at the threshold, then re-notify every further
				// threshold so the user sees the elapsed time keep growing.
				if idle >= threshold && idle-a.lastNotified >= threshold {
					first := a.lastNotified == 0
					a.lastNotified = idle
					stuckList = append(stuckList, stuck{a.label, idle, first})
				}
			}
			e.actMu.Unlock()
			for _, s := range stuckList {
				e.reportStall(s.label, s.idle, s.first)
			}
		}
	}
}

// reportStall shows the stall line for a stage and, on the first
// notification of a stall, records it as E5007: the later reminders are the
// same stall, and /diagnose errors counts stalls, not reminders. It does not
// log it either: a log line would land in errors.log a second time, uncoded,
// through the log sink.
//
// It is a plain line. It used to start with "\r\x1b[K" to wipe whatever the
// terminal showed on the current row: cursor steering that a front end with
// a live region must never receive, and garbage in a piped log.
func (e *Engine) reportStall(label string, idle time.Duration, record bool) {
	e.engineOutput(fmt.Sprintf(
		"\x1b[33m! 仍在「%s」阶段，已 %s 无进展（可能卡住，按 Ctrl+C 可中断）\x1b[0m",
		label, idle.Round(time.Second)))
	if record {
		diagnostic.ReportError(&diagnostic.Stall{Stage: label, Idle: idle}, e.diagContext("", ""))
	}
}

// localModelStallThreshold is the stall threshold for a model call served by
// a local or self-hosted provider: a CPU-bound llama.cpp spends well over 30
// seconds on a 13K prompt before its first token, and calling that a hang
// every 30 seconds was noise.
const localModelStallThreshold = 90 * time.Second

// stallThresholdFor is the idle time after which the stage named label is
// called stuck: longer for model calls to a local provider.
func (e *Engine) stallThresholdFor(label string) time.Duration {
	if strings.HasPrefix(label, modelCallActivity) && e.localProvider() {
		return localModelStallThreshold
	}
	// A build or a test suite that prints nothing for a minute is normal; a
	// go test compiling for 31 s was called "可能卡住".
	if label == toolActivity+" bash" || label == toolActivity+" powershell" {
		return shellStallThreshold
	}
	return stallThreshold
}

// toolActivity starts the label of a tool call's activity.
const toolActivity = "执行工具"

// shellStallThreshold is the stall threshold of a shell command without
// output.
const shellStallThreshold = 2 * time.Minute

// localProvider reports whether the configured provider is a local or
// self-hosted server: a loopback base URL, or a provider that only exists
// locally.
func (e *Engine) localProvider() bool {
	pc := e.config.Provider
	switch strings.ToLower(pc.Name) {
	case "ollama", "lmstudio", "llamacpp", "llama.cpp", "vllm":
		return true
	}
	return api.IsLocalBaseURL(pc.BaseURL)
}

// modelCallActivity starts the label of a model call's activity; the stall
// line shows it ("仍在「调用模型 x」阶段").
const modelCallActivity = "调用模型"
