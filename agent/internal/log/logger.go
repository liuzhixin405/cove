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

func SetLevel(l Level)              { defaultLogger.level = l }
func SetWriter(w io.Writer)         { defaultLogger.writer = w }
func NewLogger(level Level, w io.Writer) *Logger { return &Logger{level: level, writer: w} }

func (l *Logger) log(level Level, format string, args ...any) {
	if level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(l.writer, "[%s %s] %s\n", ts, levelNames[level], fmt.Sprintf(format, args...))
}

func Debugf(format string, args ...any)  { defaultLogger.log(Debug, format, args...) }
func Infof(format string, args ...any)   { defaultLogger.log(Info, format, args...) }
func Warnf(format string, args ...any)   { defaultLogger.log(Warn, format, args...) }
func Errorf(format string, args ...any)  { defaultLogger.log(Error, format, args...) }
