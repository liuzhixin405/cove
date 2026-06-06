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

func SetLevel(l Level)                           { defaultLogger.level = l }
func SetWriter(w io.Writer)                      { defaultLogger.writer = w }
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
	if level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(l.writer, "[%s %s] ", ts, levelNames[level])
	fmt.Fprintf(l.writer, format, args...)
	fmt.Fprintln(l.writer)
}

func Debugf(format string, args ...any) { defaultLogger.log(Debug, format, args...) }
func Infof(format string, args ...any)  { defaultLogger.log(Info, format, args...) }
func Warnf(format string, args ...any)  { defaultLogger.log(Warn, format, args...) }
func Errorf(format string, args ...any) { defaultLogger.log(Error, format, args...) }
