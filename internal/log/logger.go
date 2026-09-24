package log

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

var levelNames = map[Level]string{Debug: "DEBUG", Info: "INFO", Warn: "WARN", Error: "ERROR"}

type Logger struct {
	level  Level
	writer io.Writer
	mu     sync.Mutex
}

var defaultLogger = &Logger{level: Info, writer: os.Stderr}

// sinks receive every Warn/Error log line in addition to the writer. They let
// higher-level packages consume problems without this package depending on
// them (which would be an import cycle).
//
// There is a LIST rather than one slot because two independent consumers are
// legitimate at the same time: internal/diagnostic persists problems to
// ~/.cove/errors.log, and a front end displays them. With a single slot
// whichever registered last silently disabled the other — so wiring the UI
// would have stopped error persistence, with nothing to indicate it.
var (
	sinkMu sync.RWMutex
	sinks  []func(level Level, msg string)
)

// AddSink registers a callback invoked for every Warn/Error log entry.
// Multiple sinks may be registered; each is called in registration order.
// The callback must be cheap and non-blocking: it runs inline on the logging
// goroutine.
func AddSink(fn func(level Level, msg string)) {
	if fn == nil {
		return
	}
	sinkMu.Lock()
	sinks = append(sinks, fn)
	sinkMu.Unlock()
}

// SetSink replaces all registered sinks with fn. Pass nil to remove them all.
//
// Prefer AddSink: this exists for the startup path that legitimately owns the
// whole list, and replacing the list is how a caller would accidentally
// disable another package's sink.
func SetSink(fn func(level Level, msg string)) {
	sinkMu.Lock()
	if fn == nil {
		sinks = nil
	} else {
		sinks = []func(Level, string){fn}
	}
	sinkMu.Unlock()
}

// ClearSinks removes every registered sink. Tests use it to isolate.
func ClearSinks() {
	sinkMu.Lock()
	sinks = nil
	sinkMu.Unlock()
}

// SetLevel and SetWriter take the logger's mutex: they are called at startup
// and from /debug while background goroutines are already logging, and the
// fields they write are read on every log call.

func SetLevel(l Level) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.level = l
}

func SetWriter(w io.Writer) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.writer = w
}

func NewLogger(level Level, w io.Writer) *Logger { return &Logger{level: level, writer: w} }

func (l *Logger) log(level Level, format string, args ...any) {
	// The sink receives Warn/Error regardless of the writer's level filter, so
	// problems are always persisted even when the console is quiet (Info level).
	if level >= Warn {
		sinkMu.RLock()
		// Snapshot under the lock, then call outside it: a sink is free to log
		// (a UI sink might), and holding sinkMu across that would deadlock.
		fns := make([]func(Level, string), len(sinks))
		copy(fns, sinks)
		sinkMu.RUnlock()
		if len(fns) > 0 {
			msg := fmt.Sprintf(format, args...)
			for _, fn := range fns {
				fn(level, msg)
			}
		}
	}
	// The level check moved inside the lock: it reads l.level, which SetLevel
	// writes, so checking it before acquiring the mutex was a data race.
	l.mu.Lock()
	defer l.mu.Unlock()
	if level < l.level {
		return
	}
	// One Write per entry: the REPL's writer prints each Write as its own
	// line, so the old three-part write put the "[time LEVEL]" prefix and the
	// message on separate lines.
	line := fmt.Sprintf("[%s %s] ", time.Now().Format("15:04:05.000"), levelNames[level]) +
		fmt.Sprintf(format, args...) + "\n"
	_, _ = io.WriteString(l.writer, line)
}

func Debugf(format string, args ...any) { defaultLogger.log(Debug, format, args...) }
func Infof(format string, args ...any)  { defaultLogger.log(Info, format, args...) }
func Warnf(format string, args ...any)  { defaultLogger.log(Warn, format, args...) }
func Errorf(format string, args ...any) { defaultLogger.log(Error, format, args...) }
