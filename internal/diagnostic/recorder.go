package diagnostic

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

// RuntimeEvent is one recorded runtime problem (error, warning, stall, etc.).
// Unlike DiagError (which describes a known error catalogue entry), a
// RuntimeEvent is an observed occurrence captured while the agent is running so
// that a later pass — or the user via /diagnose — can review and act on it.
type RuntimeEvent struct {
	Time     time.Time `json:"time"`
	Severity Severity  `json:"severity"`
	Category Category  `json:"category"`
	Message  string    `json:"message"`
	Code     ErrorCode `json:"code,omitempty"` // set when matched to a known error
}

const maxRuntimeEvents = 200

var (
	runtimeMu     sync.Mutex
	runtimeEvents []RuntimeEvent
	runtimePath   string // resolved lazily; persistent append-only log
)

// runtimeLogPath returns (and memoizes) the path of the persistent runtime
// error log under the user's cove directory.
func runtimeLogPath() string {
	if runtimePath != "" {
		return runtimePath
	}
	// Never persist while running under `go test`: test fixtures deliberately
	// trigger errors (panics, rejected permissions, unknown tools) and must not
	// pollute the user's real ~/.cove/errors.log.
	if testing.Testing() {
		runtimePath = "-"
		return runtimePath
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		runtimePath = "-" // sentinel: persistence disabled
		return runtimePath
	}
	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		runtimePath = "-" // cannot create dir: disable persistence
		return runtimePath
	}
	runtimePath = filepath.Join(dir, "errors.log")
	return runtimePath
}

// RecordRuntime captures a runtime problem: it is appended to the in-memory
// ring buffer and to the persistent log file. The message is matched against
// the known-error catalogue so fixable problems can be surfaced later.
func RecordRuntime(sev Severity, cat Category, message string) {
	ev := RuntimeEvent{
		Time:     time.Now(),
		Severity: sev,
		Category: cat,
		Message:  message,
		Code:     matchKnownCode(cat, message),
	}

	runtimeMu.Lock()
	runtimeEvents = append(runtimeEvents, ev)
	if len(runtimeEvents) > maxRuntimeEvents {
		runtimeEvents = runtimeEvents[len(runtimeEvents)-maxRuntimeEvents:]
	}
	runtimeMu.Unlock()

	if p := runtimeLogPath(); p != "-" {
		if line, err := json.Marshal(ev); err == nil {
			if f, ferr := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
				_, _ = f.Write(append(line, '\n'))
				_ = f.Close()
			}
		}
	}
}

// AttachToLogger wires the log package so every Warn/Error log entry is also
// persisted to the runtime error log. Call once at startup. This ensures
// background-task failures (which previously only used log.*f to a possibly
// silent stderr) are captured in the file for later troubleshooting.
func AttachToLogger() {
	log.SetSink(func(level log.Level, msg string) {
		sev := SevWarning
		if level >= log.Error {
			sev = SevError
		}
		RecordRuntime(sev, CatEngine, msg)
	})
}

// matchKnownCode does a best-effort match of a free-form message to a known
// error definition by scanning category-matching defs for keyword overlap.
func matchKnownCode(cat Category, message string) ErrorCode {
	msg := strings.ToLower(message)
	for code, def := range registry {
		if def.Category != cat {
			continue
		}
		key := strings.ToLower(def.Message)
		if key != "" && strings.Contains(msg, key) {
			return code
		}
	}
	return ""
}

// RecentRuntime returns a copy of the recorded runtime events, newest last.
func RecentRuntime() []RuntimeEvent {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	out := make([]RuntimeEvent, len(runtimeEvents))
	copy(out, runtimeEvents)
	return out
}

// LoadRuntimeLog reads the persistent runtime log so events from previous runs
// are available (e.g. to /diagnose right after a restart following a hang).
func LoadRuntimeLog() []RuntimeEvent {
	p := runtimeLogPath()
	if p == "-" {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var events []RuntimeEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev RuntimeEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events
}

// RuntimeSummary groups recent runtime events and pairs each recurring problem
// with a fix suggestion drawn from the known-error catalogue. It is used by the
// self-check reminder ("提醒要修复") so users see actionable next steps.
type RuntimeSummary struct {
	Message  string
	Count    int
	Severity Severity
	Recovery string // fix hint if the problem matched a known auto/manual fix
	Fixable  bool   // a known auto-fix exists for this problem
}

// SummarizeRuntime aggregates the given events by message and attaches recovery
// hints, ordered by severity then frequency (most actionable first).
func SummarizeRuntime(events []RuntimeEvent) []RuntimeSummary {
	type agg struct {
		sum RuntimeSummary
	}
	byMsg := map[string]*agg{}
	for _, ev := range events {
		a, ok := byMsg[ev.Message]
		if !ok {
			a = &agg{sum: RuntimeSummary{Message: ev.Message, Severity: ev.Severity}}
			if ev.Code != "" {
				if def := registry[ev.Code]; def != nil {
					a.sum.Recovery = def.Recovery
					a.sum.Fixable = def.AutoFixable
				}
			}
			byMsg[ev.Message] = a
		}
		a.sum.Count++
		if ev.Severity > a.sum.Severity {
			a.sum.Severity = ev.Severity
		}
	}
	out := make([]RuntimeSummary, 0, len(byMsg))
	for _, a := range byMsg {
		out = append(out, a.sum)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		return out[i].Count > out[j].Count
	})
	return out
}

// runtimeArchiveDir returns the directory where archived logs are stored.
func runtimeArchiveDir() string {
	p := runtimeLogPath()
	if p == "-" {
		return "-"
	}
	dir := filepath.Join(filepath.Dir(p), "errors-archive")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// ArchiveRuntimeLog moves the current runtime log into the archive directory
// (timestamped) and clears the in-memory buffer, beginning a fresh error cycle.
// It returns the archive path, or an empty string if there was nothing to
// archive. Call this once the reported problems have been fixed.
func ArchiveRuntimeLog() (string, error) {
	p := runtimeLogPath()
	if p == "-" {
		return "", nil
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			// Nothing persisted yet; still clear memory for a clean cycle.
			runtimeMu.Lock()
			runtimeEvents = nil
			runtimeMu.Unlock()
			return "", nil
		}
		return "", err
	}
	dir := runtimeArchiveDir()
	if dir == "-" {
		return "", nil
	}
	dest := filepath.Join(dir, "errors-"+time.Now().Format("20060102-150405")+".log")
	if err := os.Rename(p, dest); err != nil {
		return "", err
	}
	runtimeMu.Lock()
	runtimeEvents = nil
	runtimeMu.Unlock()
	return dest, nil
}
