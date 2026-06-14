package command

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/permission"
)

func humanBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func configValue(cfg *config.Config, key string) string {
	switch key {
	case "model":
		return cfg.Model
	case "provider":
		return cfg.Provider.Name
	case "api_key", "api-key":
		if cfg.Provider.APIKey == "" {
			return ""
		}
		return "[REDACTED]"
	case "base_url", "base-url":
		return cfg.Provider.BaseURL
	case "mode", "permission_mode", "permission-mode":
		return cfg.PermissionMode
	case "budget", "max_budget_usd", "max-budget-usd":
		return fmt.Sprintf("%.2f", cfg.MaxBudgetUsd)
	case "system", "system_prompt", "system-prompt":
		return cfg.SystemPrompt
	default:
		return ""
	}
}

func applyConfigValue(cfg *config.Config, key, value string) error {
	switch key {
	case "model":
		cfg.Model = value
	case "provider":
		cfg.Provider.Name = value
	case "api_key", "api-key":
		cfg.Provider.APIKey = value
	case "base_url", "base-url":
		cfg.Provider.BaseURL = value
	case "mode", "permission_mode", "permission-mode":
		if !permission.ValidMode(permission.Mode(value)) {
			return fmt.Errorf("invalid permission mode: %s", value)
		}
		cfg.PermissionMode = value
	case "budget", "max_budget_usd", "max-budget-usd":
		var v float64
		if _, err := fmt.Sscanf(value, "%f", &v); err != nil {
			return fmt.Errorf("invalid budget: %w", err)
		}
		cfg.MaxBudgetUsd = v
	case "system", "system_prompt", "system-prompt":
		cfg.SystemPrompt = value
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

func renderConfig(cfg *config.Config) string {
	pc := cfg.EffectiveProvider()
	data, _ := json.MarshalIndent(map[string]any{
		"model":           cfg.Model,
		"provider":        pc.Name,
		"base_url":        pc.BaseURL,
		"permission_mode": cfg.PermissionMode,
		"max_budget_usd":  cfg.MaxBudgetUsd,
		"thinking_tokens": cfg.ThinkingTokens,
		"debug":           cfg.Debug,
		"api_key_set":     pc.APIKey != "",
		"system_prompt":   cfg.SystemPrompt,
		"mcp_servers":     len(cfg.MCPServers),
	}, "", "  ")
	return string(data)
}

func recentFailureClues(msgs []api.Message, limit int) []string {
	if limit <= 0 || len(msgs) == 0 {
		return nil
	}

	out := make([]string, 0, limit)
	seen := make(map[string]bool, limit)

	for i := len(msgs) - 1; i >= 0 && len(out) < limit; i-- {
		m := msgs[i]
		if m.Content == "" {
			continue
		}
		if !looksLikeFailureMessage(m) {
			continue
		}
		clue := summarizeFailureClue(m)
		if clue == "" || seen[clue] {
			continue
		}
		seen[clue] = true
		out = append(out, clue)
	}

	return out
}

func looksLikeFailureMessage(m api.Message) bool {
	text := strings.ToLower(strings.TrimSpace(m.Content))
	if text == "" {
		return false
	}
	if m.Role == "tool" && strings.HasPrefix(text, "error:") {
		return true
	}
	keywords := []string{
		"error:", "failed", "failure", "panic", "exception", "traceback",
		"timeout", "timed out", "denied", "forbidden", "not found", "no such",
		"invalid", "unable to", "cannot",
		"错误", "失败", "异常", "超时", "未找到", "无效", "拒绝",
	}
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func summarizeFailureClue(m api.Message) string {
	line := firstNonEmptyLine(m.Content)
	if line == "" {
		return ""
	}
	line = strings.TrimSpace(line)
	line = trimRunes(line, 140)

	source := ""
	if m.Role == "tool" {
		if m.Name != "" {
			source = fmt.Sprintf("[%s] ", m.Name)
		} else {
			source = "[tool] "
		}
	}
	return source + line
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func trimRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func failureActionHint(clues []string) string {
	if len(clues) == 0 {
		return "先执行 /doctor 验证环境，再排查具体错误信息。"
	}
	text := strings.ToLower(strings.Join(clues, " | "))
	switch {
	case strings.Contains(text, "denied") || strings.Contains(text, "forbidden") || strings.Contains(text, "拒绝"):
		return "先检查权限与模式（/permissions、/mode），确认工具是否被拦截。"
	case strings.Contains(text, "not found") || strings.Contains(text, "no such") || strings.Contains(text, "未找到"):
		return "优先核对路径/文件名，再用 grep 或 ls 验证目标是否存在。"
	case strings.Contains(text, "timeout") || strings.Contains(text, "timed out") || strings.Contains(text, "超时"):
		return "先缩小输入范围并减少并发，再重试一次确认是否稳定复现。"
	case strings.Contains(text, "invalid") || strings.Contains(text, "schema") || strings.Contains(text, "无效"):
		return "先对照命令/工具参数定义，逐项校验必填字段与格式。"
	case strings.Contains(text, "panic") || strings.Contains(text, "nil"):
		return "先抓首个栈帧位置，加空值保护并补最小回归测试。"
	default:
		return "按“最小复现 -> 增加观测 -> 二分排查 -> 修复回归”顺序推进。"
	}
}

func countGitStatusLines(status string) int {
	s := strings.TrimSpace(status)
	if s == "" || s == "(clean)" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
