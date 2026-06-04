package main

import (
	"fmt"
	"strings"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/config"
	"github.com/agentgo/internal/cost"
	"github.com/agentgo/internal/engine"
	"github.com/agentgo/internal/permission"
	"github.com/agentgo/internal/state"
)

func handleBuiltinConfigCommand(input string, cfg *config.Config, eng *engine.Engine, pm *permission.Manager, as *state.AppState) bool {
	switch {
	case input == "/config":
		showConfig()
		return true
	case strings.HasPrefix(input, "/model "):
		if err := applyProviderConfigChange(cfg, eng, func() error {
			cfg.Model = config.ResolveModelForProvider(strings.TrimPrefix(input, "/model "), cfg.Provider.Name)
			as.Model = cfg.Model
			return config.Save(cfg)
		}); err != nil {
			fmt.Printf("模型更新失败: %v\n", err)
			return true
		}
		fmt.Printf("模型: %s（已保存）\n", cfg.Model)
		return true
	case strings.HasPrefix(input, "/provider "):
		providerName := strings.TrimSpace(strings.TrimPrefix(input, "/provider "))
		if !api.IsKnownProvider(providerName) {
			fmt.Printf("无效的供应商: %s\n", providerName)
			fmt.Println(providerHelpLine())
			return true
		}
		if err := applyProviderConfigChange(cfg, eng, func() error {
			cfg.Provider.Name = providerName
			return config.Save(cfg)
		}); err != nil {
			fmt.Printf("供应商更新失败: %v\n", err)
			return true
		}
		fmt.Printf("供应商: %s（已保存）\n", cfg.Provider.Name)
		return true
	case strings.HasPrefix(input, "/api-key "):
		if err := applyProviderConfigChange(cfg, eng, func() error {
			cfg.Provider.APIKey = strings.TrimSpace(strings.TrimPrefix(input, "/api-key "))
			return config.Save(cfg)
		}); err != nil {
			fmt.Printf("API 密钥更新失败: %v\n", err)
			return true
		}
		fmt.Println("API 密钥已保存")
		return true
	case strings.HasPrefix(input, "/base-url "):
		if err := applyProviderConfigChange(cfg, eng, func() error {
			cfg.Provider.BaseURL = strings.TrimSpace(strings.TrimPrefix(input, "/base-url "))
			return config.Save(cfg)
		}); err != nil {
			fmt.Printf("Base URL 更新失败: %v\n", err)
			return true
		}
		fmt.Println("Base URL 已保存")
		return true
	case strings.HasPrefix(input, "/mode "):
		m := permission.Mode(strings.TrimPrefix(input, "/mode "))
		if permission.ValidMode(m) {
			pm.SetMode(m)
			eng.SetPermissionMode(m)
			cfg.PermissionMode = string(m)
			as.PermissionMode = string(m)
			config.Save(cfg)
			fmt.Printf("模式: %s\n", m)
		} else {
			fmt.Printf("无效模式。可选: %s\n", permission.Modes())
		}
		return true
	case strings.HasPrefix(input, "/budget "):
		handleBudgetCommand(input, cfg, eng, as)
		return true
	case input == "/cost":
		fmt.Println("本次会话:", eng.CostTracker().Summary())
		ch := cost.NewCostHistory()
		if len(ch.Records) > 0 {
			fmt.Printf("近 24小时: $%.4f | 近 7天: $%.4f | 总计: $%.4f (%d 个会话)\n",
				ch.Last24Hours(), ch.Last7Days(), ch.TotalAllTime(), len(ch.Records))
		}
		return true
	default:
		return false
	}
}

func handleBudgetCommand(input string, cfg *config.Config, eng *engine.Engine, as *state.AppState) {
	arg := strings.TrimSpace(strings.TrimPrefix(input, "/budget "))
	if strings.EqualFold(arg, "auto") {
		b := cfg.MaxBudgetUsd
		if tr := eng.CostTracker(); tr != nil {
			suggested := tr.SuggestedBudget()
			if suggested > b {
				b = suggested
			}
		}
		if b > 0 {
			cfg.MaxBudgetUsd = b
			as.MaxBudget = b
			eng.SetMaxBudget(b)
			config.Save(cfg)
			fmt.Printf("预算已自动调整到: $%.2f\n", b)
		}
		return
	}
	var b float64
	fmt.Sscanf(arg, "%f", &b)
	if b > 0 {
		cfg.MaxBudgetUsd = b
		as.MaxBudget = b
		eng.SetMaxBudget(b)
		config.Save(cfg)
		fmt.Printf("预算: $%.2f\n", b)
	}
}
