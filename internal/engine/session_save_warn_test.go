package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/session"
)

// A failed session save is logged, not dropped: the conversation would
// otherwise be missing from /resume with no hint why.
func TestSaveSessionFailureIsLogged(t *testing.T) {
	// The sessions "directory" is a regular file, so every save fails.
	notDir := filepath.Join(t.TempDir(), "sessions")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var warnings []string
	active := true
	log.AddSink(func(level log.Level, msg string) {
		mu.Lock()
		defer mu.Unlock()
		if active && level == log.Warn {
			warnings = append(warnings, msg)
		}
	})
	t.Cleanup(func() { mu.Lock(); active = false; mu.Unlock() })

	eng := newTestEngine(&mockProvider{})
	eng.store = session.NewStoreAt(notDir)
	eng.session = &session.Record{ID: "s-save-fail", Title: "t"}
	eng.saveSession()

	mu.Lock()
	defer mu.Unlock()
	for _, w := range warnings {
		if strings.Contains(w, "session save failed") {
			return
		}
	}
	t.Fatalf("no \"session save failed\" warning; got %q", warnings)
}
