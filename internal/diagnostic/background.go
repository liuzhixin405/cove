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

// BackgroundStatusFn, when set, supplies the background-learning state of the
// running session (the engine sets it to its dream runner and memory store).
// Unset, the checker reads the same state from disk, which is what a process
// with no engine (cove doctor) can see.
var BackgroundStatusFn func() BackgroundStatus

// PolicyLoadErrorFn, when set, reports why the running engine could not load
// policies.json (nil when it loaded or does not exist). Unset, the checker
// parses the file in the config directory itself.
var PolicyLoadErrorFn func() error

// BackgroundSummary renders the "后台学习" and "权限规则文件" items as they
// appear in the full report, for /doctor (which does not run the other,
// slower checks). It honors BackgroundStatusFn and PolicyLoadErrorFn.
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
	case BackgroundStatusFn != nil:
		return BackgroundStatusFn()
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
	case PolicyLoadErrorFn != nil:
		return PolicyLoadErrorFn()
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
