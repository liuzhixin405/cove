package diagnostic

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/config"
)

// CheckResult represents one diagnostic check's outcome.
type CheckResult struct {
	Name    string     // Check name (short, English identifier)
	Title   string     // User-facing title (Chinese)
	Status  Severity   // SevInfo=pass, SevWarning/SevError/SevFatal=problem found
	Error   *DiagError // Structured error if problem found
	Skipped bool       // Check was skipped (e.g. not applicable on this OS)
}

// Report aggregates all diagnostic results.
type Report struct {
	Results   []CheckResult
	AutoFixed int
	Duration  time.Duration
}

// HasProblems returns true if any check found issues.
func (r *Report) HasProblems() bool {
	for _, res := range r.Results {
		if res.Status >= SevWarning && (res.Error == nil || !res.Error.Fixed) {
			return true
		}
	}
	return false
}

// Summary returns a one-line status string.
func (r *Report) Summary() string {
	total := len(r.Results)
	passed := 0
	warnings := 0
	errors := 0
	fixed := 0
	for _, res := range r.Results {
		if res.Skipped {
			continue
		}
		switch {
		case res.Error != nil && res.Error.Fixed:
			fixed++
		case res.Status <= SevInfo:
			passed++
		case res.Status == SevWarning:
			warnings++
		default:
			errors++
		}
	}
	parts := []string{fmt.Sprintf("共 %d 项检查", total)}
	if passed > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[32m%d 通过\x1b[0m", passed))
	}
	if fixed > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[32m%d 已修复\x1b[0m", fixed))
	}
	if warnings > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[33m%d 警告\x1b[0m", warnings))
	}
	if errors > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[31m%d 错误\x1b[0m", errors))
	}
	return strings.Join(parts, ", ")
}

// Format returns the full diagnostic report as a terminal-ready string.
func (r *Report) Format() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\x1b[1m🔍 系统诊断报告\x1b[0m (耗时 %s)\n", r.Duration.Round(time.Millisecond)))
	sb.WriteString(fmt.Sprintf("   %s\n\n", r.Summary()))

	for _, res := range r.Results {
		if res.Skipped {
			continue
		}
		icon := "\x1b[32m✓\x1b[0m"
		if res.Error != nil && res.Error.Fixed {
			icon = "\x1b[32m🔧\x1b[0m"
		} else if res.Status == SevWarning {
			icon = "\x1b[33m⚠\x1b[0m"
		} else if res.Status >= SevError {
			icon = "\x1b[31m✗\x1b[0m"
		}

		sb.WriteString(fmt.Sprintf(" %s %s", icon, res.Title))
		if res.Error != nil {
			sb.WriteString("\n")
			// Indent error details
			lines := strings.Split(res.Error.Format(), "\n")
			for _, line := range lines {
				sb.WriteString(fmt.Sprintf("   %s\n", line))
			}
		} else {
			sb.WriteString("\n")
		}
	}

	if r.AutoFixed > 0 {
		sb.WriteString(fmt.Sprintf("\n\x1b[32m✓ 所有修复已立即生效，无需重启\x1b[0m\n"))
	}
	return sb.String()
}

// Checker runs diagnostic checks against the current environment.
type Checker struct {
	cfg     *config.Config
	homeDir string
}

// NewChecker creates a new diagnostic checker.
func NewChecker(cfg *config.Config) *Checker {
	home, _ := os.UserHomeDir()
	return &Checker{cfg: cfg, homeDir: home}
}

// RunAll executes all diagnostic checks and returns a report.
func (c *Checker) RunAll(ctx context.Context) *Report {
	start := time.Now()
	report := &Report{}

	checks := []func(context.Context) CheckResult{
		c.checkConfigExists,
		c.checkConfigValid,
		c.checkAPIKey,
		c.checkModelValid,
		c.checkNetworkReachable,
		c.checkShellAvailable,
		c.checkDataDir,
		c.checkDiskSpace,
		c.checkSessionIntegrity,
	}

	for _, check := range checks {
		result := check(ctx)
		report.Results = append(report.Results, result)
		if result.Error != nil && result.Error.Fixed {
			report.AutoFixed++
		}
	}

	report.Duration = time.Since(start)
	return report
}

// RunQuick runs only fast checks (no network) suitable for startup.
func (c *Checker) RunQuick() *Report {
	start := time.Now()
	report := &Report{}

	checks := []func(context.Context) CheckResult{
		c.checkConfigExists,
		c.checkConfigValid,
		c.checkAPIKey,
		c.checkModelValid,
		c.checkShellAvailable,
		c.checkDataDir,
	}

	ctx := context.Background()
	for _, check := range checks {
		result := check(ctx)
		report.Results = append(report.Results, result)
		if result.Error != nil && result.Error.Fixed {
			report.AutoFixed++
		}
	}

	report.Duration = time.Since(start)
	return report
}

func (c *Checker) checkConfigExists(_ context.Context) CheckResult {
	res := CheckResult{Name: "config_exists", Title: "配置文件"}
	cfgPath := filepath.Join(c.homeDir, ".cove", "config.json")

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// Try to auto-fix by creating a richer default config that matches the
		// documented shape and leaves room for the user to set their provider/api key.
		dir := filepath.Dir(cfgPath)
		if err := os.MkdirAll(dir, 0750); err == nil {
			defaultCfg := "{\n  \"debug\": false,\n  \"max_budget_usd\": 10,\n  \"model\": \"deepseek-v4-pro\",\n  \"model_fast\": \"deepseek-v4-flash\",\n  \"permission_mode\": \"default\",\n  \"provider\": {\n    \"api_key\": \"sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\",\n    \"base_url\": \"https://api.deepseek.com/v1\",\n    \"name\": \"deepseek\"\n  },\n  \"system_prompt\": \"你是 Cove，一个高效的 AI 编程助手。先理解任务，再用工具完成它；需要时读取、搜索、修改、执行、验证并给出真实结果，遇到问题要诚实说明。\",\n  \"telemetry\": true,\n  \"thinking_tokens\": 16000,\n  \"verbose\": false\n}\n"
			if err := os.WriteFile(cfgPath, []byte(defaultCfg), 0640); err == nil {
				res.Status = SevRecovered
				res.Error = NewFixed(ErrConfigMissing, cfgPath)
				return res
			}
		}
		res.Status = SevFatal
		res.Error = New(ErrConfigMissing, cfgPath)
	} else {
		res.Status = SevInfo
	}
	return res
}

func (c *Checker) checkConfigValid(_ context.Context) CheckResult {
	res := CheckResult{Name: "config_valid", Title: "配置格式"}
	if c.cfg == nil {
		res.Status = SevFatal
		res.Error = New(ErrConfigInvalid, "config is nil")
		return res
	}
	res.Status = SevInfo
	return res
}

func (c *Checker) checkAPIKey(_ context.Context) CheckResult {
	res := CheckResult{Name: "api_key", Title: "API Key"}
	if c.cfg == nil {
		res.Skipped = true
		return res
	}

	provName := c.cfg.Provider.Name
	if provName == "" {
		res.Status = SevFatal
		res.Error = New(ErrConfigProviderEmpty)
		return res
	}

	// Check various sources for API key
	hasKey := c.cfg.Provider.APIKey != "" ||
		len(c.cfg.Provider.APIKeys) > 0 ||
		os.Getenv("LLM_API_KEY") != "" ||
		os.Getenv("DEEPSEEK_API_KEY") != "" ||
		os.Getenv("OPENAI_API_KEY") != "" ||
		os.Getenv("ANTHROPIC_API_KEY") != ""

	if !hasKey {
		res.Status = SevFatal
		res.Error = New(ErrConfigAPIKeyMissing, provName)
	} else {
		res.Status = SevInfo
	}
	return res
}

func (c *Checker) checkModelValid(_ context.Context) CheckResult {
	res := CheckResult{Name: "model_valid", Title: "模型配置"}
	if c.cfg == nil {
		res.Skipped = true
		return res
	}

	model := c.cfg.Model
	if model == "" || strings.EqualFold(model, "auto") {
		// Auto-resolved at runtime, always valid
		res.Status = SevInfo
		return res
	}

	// Basic sanity: model name should not contain spaces or special chars
	if strings.ContainsAny(model, " \t\n{}[]") {
		res.Status = SevError
		res.Error = New(ErrConfigModelInvalid, model)
		return res
	}

	res.Status = SevInfo
	return res
}

func (c *Checker) checkNetworkReachable(ctx context.Context) CheckResult {
	res := CheckResult{Name: "network", Title: "API 网络连通"}
	if c.cfg == nil || c.cfg.Provider.BaseURL == "" {
		res.Skipped = true
		return res
	}

	baseURL := c.cfg.Provider.BaseURL
	// Extract host from base URL
	host := baseURL
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if idx := strings.IndexByte(host, '/'); idx > 0 {
		host = host[:idx]
	}
	if !strings.Contains(host, ":") {
		host += ":443"
	}

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", host)
	if err != nil {
		res.Status = SevError
		res.Error = New(ErrAPIUnreachable, baseURL)
		return res
	}
	conn.Close()

	// Quick TLS handshake test
	tlsCtx, tlsCancel := context.WithTimeout(ctx, 5*time.Second)
	defer tlsCancel()

	tlsConn, err := (&tls.Dialer{}).DialContext(tlsCtx, "tcp", host)
	if err != nil {
		res.Status = SevWarning
		res.Error = New(ErrAPIUnreachable, fmt.Sprintf("%s (TLS失败: %v)", baseURL, err))
		return res
	}
	tlsConn.Close()

	res.Status = SevInfo
	return res
}

func (c *Checker) checkShellAvailable(_ context.Context) CheckResult {
	res := CheckResult{Name: "shell", Title: "Shell 可用性"}

	if runtime.GOOS == "windows" {
		// Check PowerShell
		shells := []string{"pwsh", "powershell"}
		found := false
		for _, sh := range shells {
			if _, err := exec.LookPath(sh); err == nil {
				found = true
				break
			}
		}
		if !found {
			res.Status = SevError
			res.Error = New(ErrToolShellMiss, "powershell/pwsh")
			return res
		}
	} else {
		shells := []string{"bash", "sh"}
		found := false
		for _, sh := range shells {
			if _, err := exec.LookPath(sh); err == nil {
				found = true
				break
			}
		}
		if !found {
			res.Status = SevError
			res.Error = New(ErrToolShellMiss, "bash/sh")
			return res
		}
	}

	res.Status = SevInfo
	return res
}

func (c *Checker) checkDataDir(_ context.Context) CheckResult {
	res := CheckResult{Name: "data_dir", Title: "数据目录"}
	dataDir := filepath.Join(c.homeDir, ".cove")

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		// Try to create
		if err := os.MkdirAll(dataDir, 0750); err != nil {
			res.Status = SevError
			res.Error = New(ErrFSPermission, dataDir)
			return res
		}
		res.Status = SevInfo
		return res
	}

	// Test write access
	testFile := filepath.Join(dataDir, ".diag_test")
	if err := os.WriteFile(testFile, []byte("test"), 0640); err != nil {
		res.Status = SevError
		res.Error = New(ErrFSPermission, dataDir)
		return res
	}
	os.Remove(testFile)

	res.Status = SevInfo
	return res
}

func (c *Checker) checkDiskSpace(_ context.Context) CheckResult {
	res := CheckResult{Name: "disk_space", Title: "磁盘空间"}

	// Use a simple write test with a small file — cross-platform
	dataDir := filepath.Join(c.homeDir, ".cove")
	testFile := filepath.Join(dataDir, ".space_test")

	// Try writing a small test file
	testData := make([]byte, 4096)
	if err := os.WriteFile(testFile, testData, 0640); err != nil {
		res.Status = SevFatal
		res.Error = New(ErrFSDiskFull, "无法写入测试文件")
		return res
	}
	os.Remove(testFile)

	res.Status = SevInfo
	return res
}

func (c *Checker) checkSessionIntegrity(_ context.Context) CheckResult {
	res := CheckResult{Name: "sessions", Title: "会话完整性"}
	sessDir := filepath.Join(c.homeDir, ".cove", "sessions")

	if _, err := os.Stat(sessDir); os.IsNotExist(err) {
		// No sessions yet, that's fine
		res.Status = SevInfo
		return res
	}

	entries, err := os.ReadDir(sessDir)
	if err != nil {
		res.Status = SevWarning
		res.Error = New(ErrSessionCorrupt, "无法读取会话目录")
		return res
	}

	corrupt := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(sessDir, entry.Name())
		info, err := entry.Info()
		if err != nil || info.Size() == 0 {
			corrupt++
			// Auto-fix: remove empty/corrupt session files
			os.Remove(path)
		}
	}

	if corrupt > 0 {
		res.Status = SevRecovered
		res.Error = NewFixed(ErrSessionCorrupt, fmt.Sprintf("%d 个损坏会话已清理", corrupt))
	} else {
		res.Status = SevInfo
	}
	return res
}

// QuickCheck performs a minimal startup check and returns errors that
// should be shown to the user immediately. Returns nil if everything is fine.
func QuickCheck(cfg *config.Config) []*DiagError {
	c := NewChecker(cfg)
	report := c.RunQuick()

	var issues []*DiagError
	for _, r := range report.Results {
		if r.Error != nil && r.Status >= SevError {
			issues = append(issues, r.Error)
		}
	}
	return issues
}

// Ensure http package is used (for potential future health-check endpoint tests)
var _ = http.StatusOK
