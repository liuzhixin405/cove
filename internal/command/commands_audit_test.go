package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/session"
	"github.com/liuzhixin405/cove/internal/state"
)

// liveEngine is a fakeEngine that also offers the optional live-update hooks
// the real engine adapter exposes.
type liveEngine struct {
	fakeEngine
	instructions []string
	workDirs     []string
	reloads      []string
	modes        []permission.Mode
	budgets      []float64
	resumed      []string
}

// /resume <id> must continue the saved session under its own ID; loading
// only its messages saved every resume as a new copy.
func TestResumeCmdContinuesSessionWhenEngineCan(t *testing.T) {
	store := &fakeSessionStore{records: map[string]session.Record{
		"abc": {ID: "abc", Title: "t", Messages: []api.Message{{Role: "user", Content: "hi"}}},
	}}
	eng := &liveEngine{}
	if _, err := NewResumeCmd().Execute(context.Background(), Input{Args: []string{"abc"}, SessionStore: store, Engine: eng}); err != nil {
		t.Fatal(err)
	}
	if len(eng.resumed) != 1 || eng.resumed[0] != "abc" {
		t.Fatalf("ResumeSession calls = %v, want [abc]", eng.resumed)
	}
	if eng.loaded != nil {
		t.Fatal("messages were loaded into a fresh session as well")
	}
}

func (l *liveEngine) SetCustomInstructions(ci string) bool {
	l.instructions = append(l.instructions, ci)
	return true
}
func (l *liveEngine) SetWorkingDir(dir string) bool {
	l.workDirs = append(l.workDirs, dir)
	return true
}
func (l *liveEngine) ReloadProvider(provider, model, baseURL, apiKey string) error {
	l.reloads = append(l.reloads, provider+"|"+model+"|"+baseURL)
	return nil
}
func (l *liveEngine) ResumeSession(r *session.Record) {
	l.resumed = append(l.resumed, r.ID)
	l.msgs = r.Messages
}
func (l *liveEngine) SetPermissionMode(m permission.Mode) { l.modes = append(l.modes, m) }
func (l *liveEngine) SetMaxBudget(b float64)              { l.budgets = append(l.budgets, b) }

type fakePermManager struct{ mode permission.Mode }

func (f *fakePermManager) Mode() permission.Mode        { return f.mode }
func (f *fakePermManager) SetMode(mode permission.Mode) { f.mode = mode }

func noSave(*config.Config) error { return nil }

// restoreWD puts the working directory back when the test ends. Call it after
// creating the temp dirs the test may cd into: cleanups run last-in first-out,
// and Windows cannot remove a directory that is still the process's cwd.
func restoreWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return wd
}

// A directory outside any repository: git's answer there is an error, which
// the git commands used to swallow and report as "no changes".
func nonRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	return dir
}

func TestCommitCmdOutsideRepoSaysSo(t *testing.T) {
	out, _ := NewCommitCmd().Execute(context.Background(), Input{Cwd: nonRepoDir(t)})
	if strings.Contains(out.Message, "没有可提交的更改") || !strings.Contains(out.Message, "git") {
		t.Fatalf("outside a repository /commit must report the git error, got %q", out.Message)
	}
}

func TestReviewCmdOutsideRepoSaysSo(t *testing.T) {
	out, _ := NewReviewCmd().Execute(context.Background(), Input{Cwd: nonRepoDir(t)})
	if strings.Contains(out.Message, "没有需要审查的更改") || !strings.Contains(out.Message, "git") {
		t.Fatalf("outside a repository /review must report the git error, got %q", out.Message)
	}
}

func TestDiffCmdOutsideRepoSaysSo(t *testing.T) {
	out, _ := NewDiffCmd().Execute(context.Background(), Input{Cwd: nonRepoDir(t)})
	if out.Message == "无差异" || !strings.Contains(out.Message, "git") {
		t.Fatalf("outside a repository /diff must report the git error, got %q", out.Message)
	}
}

// /diff showed only the unstaged diff whenever there was one, so staged
// changes were invisible exactly when both kinds existed.
func TestDiffCmdShowsStagedAndUnstaged(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := NewDiffCmd().Execute(context.Background(), Input{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Data, "+staged") || !strings.Contains(out.Data, "+unstaged") {
		t.Fatalf("expected both staged and unstaged hunks, got:\n%s", out.Data)
	}
}

// /system used to replace the whole system prompt, dropping the role, tool
// rules and project context, while the saved value is appended to the
// built-in prompt on the next start.
func TestSystemCmdDoesNotReplaceWholePrompt(t *testing.T) {
	eng := &liveEngine{}
	cfg := config.DefaultConfig()
	_, err := NewSystemCmd().Execute(context.Background(), Input{Args: []string{"用中文回答"}, Config: cfg, SaveConfig: noSave, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if eng.override != "" {
		t.Fatalf("system prompt was overridden with %q", eng.override)
	}
	if len(eng.instructions) != 1 || eng.instructions[0] != "用中文回答" {
		t.Fatalf("custom instructions not applied live: %v", eng.instructions)
	}
	if cfg.SystemPrompt != "用中文回答" {
		t.Fatalf("config not updated: %q", cfg.SystemPrompt)
	}
}

func TestSystemCmdWithoutLiveEngineSaysNextSession(t *testing.T) {
	eng := &fakeEngine{}
	out, err := NewSystemCmd().Execute(context.Background(), Input{Args: []string{"x"}, Config: config.DefaultConfig(), SaveConfig: noSave, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if eng.override != "" {
		t.Fatalf("system prompt was overridden with %q", eng.override)
	}
	if !strings.Contains(out.Message, "下次") {
		t.Fatalf("message must say when it takes effect, got %q", out.Message)
	}
}

// Windows paths routinely contain spaces ("My Projects"), and the command
// line is split on whitespace before /cd sees it.
func TestCdCmdAcceptsPathWithSpaces(t *testing.T) {
	base := t.TempDir()
	restoreWD(t)
	target := filepath.Join(base, "My Projects")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{"My", "Projects"}, Cwd: base})
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if !strings.EqualFold(filepath.Clean(wd), filepath.Clean(target)) {
		t.Fatalf("cwd = %s, want %s (%s)", wd, target, out.Message)
	}
}

func TestCdCmdStripsQuotes(t *testing.T) {
	base := t.TempDir()
	restoreWD(t)
	target := filepath.Join(base, "a b")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{`"a`, `b"`}, Cwd: base}); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if !strings.EqualFold(filepath.Clean(wd), filepath.Clean(target)) {
		t.Fatalf("cwd = %s, want %s", wd, target)
	}
}

// The engine keeps its own idea of the project directory (checkpoints, the
// session's project); /cd has to tell it, or /undo restores the old project.
func TestCdCmdTellsEngineTheNewDir(t *testing.T) {
	target := t.TempDir()
	origWD := restoreWD(t)
	eng := &liveEngine{}
	if _, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{target}, Cwd: origWD, Engine: eng}); err != nil {
		t.Fatal(err)
	}
	if len(eng.workDirs) != 1 || !strings.EqualFold(filepath.Clean(eng.workDirs[0]), filepath.Clean(target)) {
		t.Fatalf("engine working dir updates = %v, want [%s]", eng.workDirs, target)
	}
}

// Until the engine can follow a directory change, /cd must not pretend the
// whole session moved.
func TestCdCmdWarnsWhenEngineCannotFollow(t *testing.T) {
	target := t.TempDir()
	origWD := restoreWD(t)
	out, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{target}, Cwd: origWD, Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "检查点") {
		t.Fatalf("expected a note that checkpoints stay on the old directory, got %q", out.Message)
	}
}

func TestCdCmdFailedChdirDoesNotTellEngine(t *testing.T) {
	eng := &liveEngine{}
	out, _ := NewCdCmd().Execute(context.Background(), Input{Args: []string{filepath.Join(t.TempDir(), "missing")}, Cwd: t.TempDir(), Engine: eng})
	if len(eng.workDirs) != 0 {
		t.Fatalf("engine told about a directory it never entered: %v", eng.workDirs)
	}
	if !strings.Contains(out.Message, "错误") {
		t.Fatalf("expected an error message, got %q", out.Message)
	}
}

// "/config provider deepseek-typo" used to be saved as-is and break the next
// start.
func TestConfigCmdRejectsUnknownProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	before := cfg.Provider.Name
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"provider", "deepsek"}, Config: cfg, SaveConfig: noSave})
	if err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	if cfg.Provider.Name != before {
		t.Fatalf("provider changed to %q", cfg.Provider.Name)
	}
}

// "/config model x" said 已保存 but the running session kept the old model
// until restart; /model applies it immediately.
func TestConfigCmdAppliesModelToRunningEngine(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Provider.Name = "deepseek"
	eng := &liveEngine{}
	as := &state.AppState{}
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"model", "deepseek-v4-flash"}, Config: cfg, SaveConfig: noSave, Engine: eng, AppState: as})
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.reloads) != 1 || !strings.HasPrefix(eng.reloads[0], "deepseek|deepseek-v4-flash|") {
		t.Fatalf("provider reloads = %v", eng.reloads)
	}
	if as.Model != "deepseek-v4-flash" {
		t.Fatalf("app state model = %q", as.Model)
	}
}

func TestConfigCmdAppliesModeToRunningSession(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &liveEngine{}
	pm := &fakePermManager{mode: permission.Default}
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"mode", "auto"}, Config: cfg, SaveConfig: noSave, Engine: eng, PermissionManager: pm})
	if err != nil {
		t.Fatal(err)
	}
	if pm.mode != permission.Mode("auto") || len(eng.modes) != 1 || eng.modes[0] != permission.Mode("auto") {
		t.Fatalf("mode not applied live: manager=%s engine=%v", pm.mode, eng.modes)
	}
}

func TestConfigCmdAppliesBudgetToRunningEngine(t *testing.T) {
	eng := &liveEngine{}
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"budget", "3.5"}, Config: config.DefaultConfig(), SaveConfig: noSave, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.budgets) != 1 || eng.budgets[0] != 3.5 {
		t.Fatalf("budget not applied live: %v", eng.budgets)
	}
}

func TestConfigCmdWithoutLiveEngineSaysRestart(t *testing.T) {
	out, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"model", "gpt-4o"}, Config: config.DefaultConfig(), SaveConfig: noSave, Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "重启") {
		t.Fatalf("message must say the change needs a restart, got %q", out.Message)
	}
}

// /doctor's help promised a config check it never did; a missing API key is
// the most common first-run problem.
func TestDoctorCmdReportsConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Provider.Name = "deepseek"
	cfg.Provider.APIKey = ""
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("LLM_API_KEY", "")
	out, _ := NewDoctorCmd().Execute(context.Background(), Input{Cwd: t.TempDir(), Config: cfg})
	if !strings.Contains(out.Message, "deepseek") || !strings.Contains(out.Message, "API key") {
		t.Fatalf("doctor output lacks provider/API key status:\n%s", out.Message)
	}
}
