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

// sink, if set, receives every Warn/Error log line in addition to the writer.
// It lets a higher-level package (e.g. diagnostic) persist problems to a file
// without the log package taking a dependency on it (avoids an import cycle).
var (
	sinkMu sync.RWMutex
	sink   func(level Level, msg string)
)

// SetSink registers a callback invoked for every Warn/Error log entry. Pass nil
// to disable. The callback must be cheap and non-blocking; it runs inline.
func SetSink(fn func(level Level, msg string)) {
	sinkMu.Lock()
	sink = fn
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
		fn := sink
		sinkMu.RUnlock()
		if fn != nil {
			fn(level, fmt.Sprintf(format, args...))
		}
	}
	// The level check moved inside the lock: it reads l.level, which SetLevel
	// writes, so checking it before acquiring the mutex was a data race.
	l.mu.Lock()
	defer l.mu.Unlock()
	if level < l.level {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(l.writer, "[%s %s] ", ts, levelNames[level])
	fmt.Fprintf(l.writer, format, args...)
	fmt.Fprintln(l.writer)
}

func Debugf(format string, args ...any) { defaultLogger.log(Debug, format, args...) }
func Infof(format string, args ...any)  { defaultLogger.log(Info, format, args...) }
func Warnf(format string, args ...any)  { defaultLogger.log(Warn, format, args...) }
func Errorf(format string, args ...any) { defaultLogger.log(Error, format, args...) }
