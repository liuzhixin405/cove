package diagnostic

import (
	"fmt"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
)

// Context is what the place that saw an error knows about the call; every
// field may be empty.
type Context struct {
	Provider, Model, Tool string
	Attempt               int
}

// Stall is the error the engine reports when a stage has made no progress
// for its threshold. It lives here so the engine can report it without the
// diagnostic layer importing the engine.
type Stall struct {
	Stage string
	Idle  time.Duration
}

func (s *Stall) Error() string {
	return fmt.Sprintf("「%s」阶段已 %s 无进展", s.Stage, s.Idle.Round(time.Second))
}

// Runtime is what a remedy may touch at run time. The CLI injects an
// implementation; headless runs and tests leave it nil, and every remedy
// then declines.
type Runtime interface {
	// SetModelContextWindow records model's real context window for this
	// session (api.SetModelContextWindow behind it).
	SetModelContextWindow(model string, tokens int)
	// Notify shows the user one line.
	Notify(line string)
}

// WindowPersister is a Runtime that can also write a learned context window
// to the configuration, so the next start knows it without hitting the
// server's wall again. The interactive shell implements it; a remedy checks
// for it with a type assertion.
type WindowPersister interface {
	PersistModelContextWindow(model string, tokens int) error
}

// RemedyFunc is an action bound to an error code. It returns what it did
// and true when it changed something, false when it did not apply.
type RemedyFunc func(ev RuntimeEvent, rt Runtime) (applied string, ok bool)

var (
	runtimeRtMu sync.RWMutex
	runtimeRt   Runtime
)

// SetRuntime installs the Runtime remedies act through; nil disables them.
func SetRuntime(rt Runtime) {
	runtimeRtMu.Lock()
	defer runtimeRtMu.Unlock()
	runtimeRt = rt
}

// HasRuntime reports whether a Runtime is installed, i.e. remedies may act.
func HasRuntime() bool { return currentRuntime() != nil }

func currentRuntime() Runtime {
	runtimeRtMu.RLock()
	defer runtimeRtMu.RUnlock()
	return runtimeRt
}

// Classify gives err the code of its kind, and the detail to record. It
// decides by the error's type (api.Classify, Stall), never by matching
// text; "" means no known kind.
func Classify(err error, c Context) (ErrorCode, string) {
	if err == nil {
		return "", ""
	}
	if st, ok := err.(*Stall); ok {
		return ErrEngineStall, st.Error()
	}
	switch api.Classify(err) {
	case api.KindContextLength:
		return ErrAPIContextLength, err.Error()
	case api.KindProviderUnavailable:
		return ErrAPIProviderUnavailable, err.Error()
	case api.KindToolArgs:
		return ErrToolArgsInvalid, err.Error()
	case api.KindRateLimit:
		return ErrAPIRateLimit, err.Error()
	case api.KindAuth:
		return ErrAPIAuth, err.Error()
	case api.KindBadRequest:
		return ErrAPIBadRequest, err.Error()
	case api.KindServerError:
		return ErrAPIServerError, err.Error()
	case api.KindTransport:
		return ErrAPIStreamBroken, err.Error()
	case api.KindUnreachable:
		return ErrAPIUnreachable, err.Error()
	case api.KindTimeout:
		return ErrAPITimeout, err.Error()
	case api.KindCanceled:
		return ErrEngineCtxCancel, err.Error()
	}
	return "", err.Error()
}

// ReportError classifies and records err, runs the code's remedy when there is
// one and a Runtime is installed, and returns the recorded event so the
// caller can quote its code. A nil err records nothing. It never panics and
// never blocks on anything but the log file.
func ReportError(err error, c Context) RuntimeEvent {
	if err == nil {
		return RuntimeEvent{}
	}
	code, detail := Classify(err, c)
	ev := RuntimeEvent{
		Time: time.Now(), Message: detail, Code: code,
		Model: c.Model, Provider: c.Provider, Tool: c.Tool, Source: "report",
		Severity: SevError, Category: CatEngine,
	}
	if c.Tool != "" {
		ev.Category = CatTool
	}
	def := registry[code]
	if code != "" && def != nil {
		ev.Severity, ev.Category = def.Severity, def.Category
	}
	record(ev)
	if def != nil && def.Remedy != nil {
		if rt := currentRuntime(); rt != nil {
			if applied, ok := applyRemedy(def.Remedy, ev, rt); ok {
				record(RuntimeEvent{
					Time: time.Now(), Severity: SevRecovered, Category: ev.Category, Code: code,
					Message: applied, Model: c.Model, Provider: c.Provider, Tool: c.Tool, Source: "remedy",
				})
			}
		}
	}
	return ev
}

// applyRemedy runs fn and turns a panic into "did not apply".
func applyRemedy(fn RemedyFunc, ev RuntimeEvent, rt Runtime) (applied string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Warnf("diagnostic remedy for %s panicked: %v", ev.Code, r)
			applied, ok = "", false
		}
	}()
	return fn(ev, rt)
}

// ResetForTest empties the recorded events, the remedy counters and the
// Runtime. Tests in other packages use it; production code never does.
func ResetForTest() {
	runtimeMu.Lock()
	runtimeEvents = nil
	runtimeMu.Unlock()
	ResetRemedyState()
	SetRuntime(nil)
}
