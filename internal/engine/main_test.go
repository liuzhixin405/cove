package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove/internal/checkpoint"
)

// TestMain gives the package a throwaway home and config directory, so New,
// called by newTestEngine for almost every test here, never touches the
// developer's real ~/.cove:
//
//   - a deny or allow rule in the real policies.json changed what these tests
//     saw (COVE_CONFIG_DIR is where policies.json is read, see policyFilePath);
//   - every test turn saved a session into the real ~/.cove/sessions, whose
//     index grew with each run and made each save slower;
//   - checkpoints were opened in the real ~/.cove/checkpoints.
//
// Tests that exercise policies or sessions set their own (isolateHome).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cove-engine-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "engine tests: create temp home:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir)
	os.Setenv("COVE_CONFIG_DIR", filepath.Join(dir, ".cove"))
	// Tests that need checkpoints create their own manager; New's would be
	// dropped, after a "git init --bare" under every fresh test HOME.
	startupCheckpoints = func(string) (*checkpoint.Manager, error) {
		return nil, errors.New("checkpoints are not opened at startup in tests")
	}
	// Create the shared checkpoint store now, so tests that open managers in
	// parallel do not race to "git init" it.
	if work := filepath.Join(dir, "init-work"); os.MkdirAll(work, 0o700) == nil {
		_, _ = checkpoint.New(work)
	}
	code := m.Run()
	removeTree(dir)
	os.Exit(code)
}
