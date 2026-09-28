package diagnostic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/permission"
)

// BackgroundStatus is what the "后台学习" item reports: the auto-dream gates
// and last run, and the memory store with its last automatic extraction.
type BackgroundStatus struct {
	Dream  dream.Status
	Memory memory.Stats
}

// SessionFuncs is the running session the checks read, when there is one:
// its background-learning state (the engine's dream runner and memory store)
// and why it could not load policies.json (nil when it loaded or does not
// exist). Without it, or with a nil function, the checks read the same state
// from disk, which is what a process with no engine (cove doctor) can see.
type SessionFuncs struct {
	Background func() BackgroundStatus
	PolicyErr  func() error
}

// The session is set at startup and read by /diagnose and /doctor from
// command goroutines; it used to be two bare function variables.
var (
	sessionMu     sync.RWMutex
	sessionSource *SessionFuncs
)

// SetSession installs the running session's sources; nil removes them.
func SetSession(f *SessionFuncs) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	sessionSource = f
}

func currentSession() *SessionFuncs {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return sessionSource
}

// BackgroundSummary renders the "后台学习" and "权限规则文件" items as they
// appear in the full report, for /doctor (which does not run the other,
// slower checks). It reads the running session when one is set (SetSession).
func BackgroundSummary() string {
	c := NewChecker(nil)
	ctx := context.Background()
	var sb strings.Builder
	formatResult(&sb, c.checkBackgroundLearning(ctx))
	formatResult(&sb, c.checkPolicyFile(ctx))
	return sb.String()
}

func (c *Checker) backgroundStatus() BackgroundStatus {
	switch {
	case c.background != nil:
		return c.background()
	case currentSession() != nil && currentSession().Background != nil:
		return currentSession().Background()
	}
	var st BackgroundStatus
	if r := dream.Current(); r != nil {
		st.Dream = r.Status()
	} else {
		st.Dream = dream.StatusFromDisk()
	}
	st.Memory = memory.NewStoreForDirs(filepath.Join(c.homeDir, ".cove", "memory")).Stats()
	return st
}

func (c *Checker) policyLoadError() error {
	switch {
	case c.policyErr != nil:
		return c.policyErr()
	case currentSession() != nil && currentSession().PolicyErr != nil:
		return currentSession().PolicyErr()
	}
	// The same parse the engine does at startup (permission.FilePolicyStorage
	// .Load), without creating the directory.
	path := filepath.Join(c.configDir, "policies.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	var rules []permission.PolicyRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	return nil
}

func (c *Checker) checkPolicyFile(_ context.Context) CheckResult {
	res := CheckResult{Name: "policies", Title: "权限规则文件"}
	if err := c.policyLoadError(); err != nil {
		res.Status = SevError
		res.Error = New(ErrPermPolicyLoad, err.Error())
		return res
	}
	res.Status = SevInfo
	return res
}

func (c *Checker) checkBackgroundLearning(_ context.Context) CheckResult {
	res := CheckResult{Name: "background", Title: "后台学习"}
	st := c.backgroundStatus()
	res.Detail = "整理 (dream): " + st.Dream.Summary() + "\n" + memorySummary(st.Memory)
	if st.Dream.LastRunErr != nil && !st.Dream.Running {
		res.Status = SevWarning
		res.Error = New(ErrEngineDreamFailed, st.Dream.LastRunErr.Error())
		return res
	}
	res.Status = SevInfo
	return res
}

func memorySummary(m memory.Stats) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "记忆: %d 条 (%.1fKB)", m.FileCount, float64(m.TotalBytes)/1024)
	if m.LastExtractedAt.IsZero() {
		sb.WriteString("，尚无自动提取记录")
	} else {
		fmt.Fprintf(&sb, "，上次提取 %s 保存 %d 条", m.LastExtractedAt.Format("2006-01-02 15:04"), m.LastExtractedCount)
	}
	return sb.String()
}
