package log

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// restoreDefaults snapshots and restores the package-level default logger state
// and the global sink. Every test that touches package-level state must call it
// so state cannot leak into another test.
func restoreDefaults(t *testing.T) {
	t.Helper()

	defaultLogger.mu.Lock()
	oldLevel := defaultLogger.level
	oldWriter := defaultLogger.writer
	defaultLogger.mu.Unlock()

	sinkMu.RLock()
	oldSink := sink
	sinkMu.RUnlock()

	t.Cleanup(func() {
		defaultLogger.mu.Lock()
		defaultLogger.level = oldLevel
		defaultLogger.writer = oldWriter
		defaultLogger.mu.Unlock()

		sinkMu.Lock()
		sink = oldSink
		sinkMu.Unlock()
	})
}

// syncBuffer is a writer that is safe for the -race tests. The logger already
// serializes writes under its own mutex, but SetWriter swaps the writer, so a
// writer can legitimately be handed to two logging goroutines around the swap.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestLevelFilterDropsBelowThreshold(t *testing.T) {
	cases := []struct {
		name      string
		level     Level
		wantLines map[Level]bool // level -> should be written
	}{
		{
			name:  "info level drops debug",
			level: Info,
			wantLines: map[Level]bool{
				Debug: false,
				Info:  true,
				Warn:  true,
				Error: true,
			},
		},
		{
			name:  "error level drops all but error",
			level: Error,
			wantLines: map[Level]bool{
				Debug: false,
				Info:  false,
				Warn:  false,
				Error: true,
			},
		},
		{
			name:  "debug level keeps everything",
			level: Debug,
			wantLines: map[Level]bool{
				Debug: true,
				Info:  true,
				Warn:  true,
				Error: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for lvl, want := range tc.wantLines {
				var buf bytes.Buffer
				l := NewLogger(tc.level, &buf)
				l.log(lvl, "payload-%d", 7)

				got := buf.Len() > 0
				if got != want {
					t.Fatalf("logger at %s: %s written=%v, want %v (output %q)",
						levelNames[tc.level], levelNames[lvl], got, want, buf.String())
				}
				if want && !strings.Contains(buf.String(), "payload-7") {
					t.Fatalf("logger at %s: %s output %q missing formatted message",
						levelNames[tc.level], levelNames[lvl], buf.String())
				}
			}
		})
	}
}

func TestFormatIncludesTimestampLevelAndMessage(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(Debug, &buf)

	l.log(Warn, "disk %s at %d%%", "full", 93)

	got := buf.String()
	// "[15:04:05.000 WARN] disk full at 93%\n"
	want := regexp.MustCompile(`^\[\d{2}:\d{2}:\d{2}\.\d{3} WARN\] disk full at 93%\n$`)
	if !want.MatchString(got) {
		t.Fatalf("log line %q does not match %v", got, want)
	}
}

func TestFormatEmitsEachLevelName(t *testing.T) {
	for lvl, name := range levelNames {
		var buf bytes.Buffer
		l := NewLogger(Debug, &buf)
		l.log(lvl, "msg")
		if !strings.Contains(buf.String(), " "+name+"] ") {
			t.Errorf("level %d: output %q does not contain level name %q", lvl, buf.String(), name)
		}
	}
}

func TestPackageLevelHelpersUseSetWriterAndSetLevel(t *testing.T) {
	restoreDefaults(t)

	var buf bytes.Buffer
	SetWriter(&buf)
	SetLevel(Info)

	Debugf("dropped-debug")
	if buf.Len() != 0 {
		t.Fatalf("Debugf at Info level wrote %q, want nothing", buf.String())
	}

	Infof("kept-info %d", 1)
	Warnf("kept-warn")
	Errorf("kept-error")

	out := buf.String()
	for _, want := range []string{"INFO] kept-info 1", "WARN] kept-warn", "ERROR] kept-error"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "dropped-debug") {
		t.Errorf("output %q contains the filtered Debug line", out)
	}

	// SetLevel must take effect on subsequent calls.
	buf.Reset()
	SetLevel(Debug)
	Debugf("now-visible")
	if !strings.Contains(buf.String(), "DEBUG] now-visible") {
		t.Fatalf("after SetLevel(Debug), Debugf wrote %q", buf.String())
	}

	// SetWriter must redirect subsequent calls away from the old writer.
	var second bytes.Buffer
	SetWriter(&second)
	buf.Reset()
	Errorf("to-second-writer")
	if buf.Len() != 0 {
		t.Errorf("old writer still received %q after SetWriter", buf.String())
	}
	if !strings.Contains(second.String(), "to-second-writer") {
		t.Errorf("new writer got %q, want the log line", second.String())
	}
}

func TestSinkReceivesWarnAndErrorRegardlessOfLevelFilter(t *testing.T) {
	restoreDefaults(t)

	type entry struct {
		level Level
		msg   string
	}
	var mu sync.Mutex
	var got []entry
	SetSink(func(l Level, msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, entry{l, msg})
	})

	// Level Error: the writer must drop Warn, but the sink must still see it.
	var buf bytes.Buffer
	SetWriter(&buf)
	SetLevel(Error)

	Debugf("debug-msg")
	Infof("info-msg")
	Warnf("warn-%s", "msg")
	Errorf("error-%s", "msg")

	mu.Lock()
	defer mu.Unlock()

	want := []entry{{Warn, "warn-msg"}, {Error, "error-msg"}}
	if len(got) != len(want) {
		t.Fatalf("sink received %+v, want exactly %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sink entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The writer's own level filter is independent and still applied.
	if strings.Contains(buf.String(), "warn-msg") {
		t.Errorf("writer at Error level wrote the Warn line: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "error-msg") {
		t.Errorf("writer at Error level dropped the Error line: %q", buf.String())
	}
}

func TestSinkAlsoFiresForNonDefaultLoggers(t *testing.T) {
	restoreDefaults(t)

	var mu sync.Mutex
	var levels []Level
	SetSink(func(l Level, _ string) {
		mu.Lock()
		defer mu.Unlock()
		levels = append(levels, l)
	})

	// The sink is global, not per-logger: a logger built with NewLogger must
	// also feed it, otherwise problems logged through a scoped logger would
	// never be persisted.
	l := NewLogger(Error, io.Discard)
	l.log(Warn, "scoped warn")
	l.log(Info, "scoped info")

	mu.Lock()
	defer mu.Unlock()
	if len(levels) != 1 || levels[0] != Warn {
		t.Fatalf("sink got %v, want exactly [Warn]", levels)
	}
}

func TestSetSinkNilDisablesSink(t *testing.T) {
	restoreDefaults(t)
	SetWriter(io.Discard)
	SetLevel(Debug)

	var mu sync.Mutex
	calls := 0
	SetSink(func(Level, string) {
		mu.Lock()
		defer mu.Unlock()
		calls++
	})

	Errorf("first")
	SetSink(nil)
	Errorf("second")
	Warnf("third")

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("sink called %d times, want 1 (only the call before SetSink(nil))", calls)
	}
}

// TestConcurrentLoggingWithSetLevelAndSetWriter is a regression test for the
// data race where (*Logger).log read l.level and l.writer outside the mutex
// that SetLevel/SetWriter take. It must be clean under -race.
func TestConcurrentLoggingWithSetLevelAndSetWriter(t *testing.T) {
	restoreDefaults(t)

	const (
		loggers  = 8
		perGoro  = 200
		mutators = 4
	)

	first := &syncBuffer{}
	SetWriter(first)
	SetLevel(Debug)

	var sinkCalls int64
	var sinkMuLocal sync.Mutex
	SetSink(func(Level, string) {
		sinkMuLocal.Lock()
		sinkCalls++
		sinkMuLocal.Unlock()
	})

	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < loggers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for j := 0; j < perGoro; j++ {
				Debugf("d %d/%d", id, j)
				Infof("i %d/%d", id, j)
				Warnf("w %d/%d", id, j)
				Errorf("e %d/%d", id, j)
			}
		}(i)
	}

	writers := make([]*syncBuffer, mutators)
	for i := 0; i < mutators; i++ {
		writers[i] = &syncBuffer{}
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			levels := []Level{Debug, Info, Warn, Error}
			for j := 0; j < perGoro; j++ {
				SetLevel(levels[j%len(levels)])
				SetWriter(writers[id])
			}
		}(i)
	}

	close(start)
	wg.Wait()

	// Warn and Error bypass the level filter on their way to the sink, so the
	// sink count is exact and independent of the racing SetLevel calls.
	sinkMuLocal.Lock()
	defer sinkMuLocal.Unlock()
	if want := int64(loggers * perGoro * 2); sinkCalls != want {
		t.Fatalf("sink saw %d Warn/Error entries, want %d", sinkCalls, want)
	}

	// Something must actually have been written somewhere.
	total := len(first.String())
	for _, w := range writers {
		total += len(w.String())
	}
	if total == 0 {
		t.Fatal("no log output was written by any goroutine")
	}
}

func TestLevelConstantsAreOrdered(t *testing.T) {
	// The filter in log() is a numeric comparison, so the constant order is
	// part of the contract: a reordering would silently invert filtering.
	if !(Debug < Info && Info < Warn && Warn < Error) {
		t.Fatalf("level constants out of order: Debug=%d Info=%d Warn=%d Error=%d", Debug, Info, Warn, Error)
	}
	if got := fmt.Sprint(levelNames[Debug], levelNames[Info], levelNames[Warn], levelNames[Error]); got != "DEBUGINFOWARNERROR" {
		t.Fatalf("level names = %q", got)
	}
}
