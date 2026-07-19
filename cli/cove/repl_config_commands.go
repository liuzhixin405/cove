package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/cost"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/state"
)

func handleBuiltinConfigCommand(input string, cfg *config.Config, eng *engine.Engine, pm *permission.Manager, as *state.AppState) bool {
	switch {
	case input == "/config":
		showConfig()
		return true
	case input == "/profile" || strings.HasPrefix(input, "/profile "):
		handleProfileCommand(input, cfg, eng, pm, as)
		return true
	case input == "/record" || strings.HasPrefix(input, "/record "):
		handleRecordCommand(input, eng)
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

func handleProfileCommand(input string, cfg *config.Config, eng *engine.Engine, pm *permission.Manager, as *state.AppState) {
	args := strings.Fields(strings.TrimSpace(strings.TrimPrefix(input, "/profile")))
	if len(args) == 0 || strings.EqualFold(args[0], "list") {
		names := make([]string, 0, len(cfg.Profiles))
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			fmt.Println("当前没有已保存的 profile。")
			return
		}
		fmt.Println("profiles:")
		for _, name := range names {
			mark := " "
			if strings.EqualFold(cfg.ActiveProfile, name) {
				mark = "*"
			}
			fmt.Printf("  %s %s\n", mark, name)
		}
		return
	}

	sub := strings.ToLower(args[0])
	if len(args) < 2 {
		fmt.Println("用法: /profile list | /profile switch <name> | /profile save <name> | /profile delete <name> | /profile show <name>")
		return
	}
	name := strings.TrimSpace(args[1])
	if name == "" {
		fmt.Println("profile 名称不能为空")
		return
	}

	switch sub {
	case "switch":
		if cfg.Profiles == nil {
			fmt.Printf("profile %s 不存在\n", name)
			return
		}
		if _, ok := cfg.Profiles[name]; !ok {
			fmt.Printf("profile %s 不存在\n", name)
			return
		}
		cfg.ActiveProfile = name
		if err := config.Save(cfg); err != nil {
			fmt.Printf("保存 active_profile 失败: %v\n", err)
			return
		}
		loaded, err := config.LoadWithProfile(name)
		if err != nil {
			fmt.Printf("加载 profile 失败: %v\n", err)
			return
		}
		if err := applyProviderConfigChange(cfg, eng, func() error {
			*cfg = *loaded
			as.Model = cfg.Model
			as.ModelFast = cfg.ModelFast
			as.MaxBudget = cfg.MaxBudgetUsd
			as.PermissionMode = cfg.PermissionMode
			return nil
		}); err != nil {
			fmt.Printf("应用 profile 失败: %v\n", err)
			return
		}
		if mode := permission.Mode(cfg.PermissionMode); permission.ValidMode(mode) {
			pm.SetMode(mode)
			eng.SetPermissionMode(mode)
		}
		eng.SetMaxBudget(cfg.MaxBudgetUsd)
		fmt.Printf("已切换到 profile: %s\n", name)
	case "save":
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]*config.Profile{}
		}
		p := &config.Profile{
			Model:          cfg.Model,
			ModelFast:      cfg.ModelFast,
			Provider:       &config.ProviderConfig{Name: cfg.Provider.Name, APIKey: cfg.Provider.APIKey, BaseURL: cfg.Provider.BaseURL},
			PermissionMode: cfg.PermissionMode,
			MaxBudgetUsd:   cfg.MaxBudgetUsd,
			ThinkingTokens: cfg.ThinkingTokens,
			Debug:          cfg.Debug,
			Verbose:        cfg.Verbose,
			SystemPrompt:   cfg.SystemPrompt,
		}
		cfg.Profiles[name] = p
		if err := config.Save(cfg); err != nil {
			fmt.Printf("保存 profile 失败: %v\n", err)
			return
		}
		fmt.Printf("profile 已保存: %s\n", name)
	case "delete":
		if cfg.Profiles == nil {
			fmt.Printf("profile %s 不存在\n", name)
			return
		}
		if _, ok := cfg.Profiles[name]; !ok {
			fmt.Printf("profile %s 不存在\n", name)
			return
		}
		delete(cfg.Profiles, name)
		if strings.EqualFold(cfg.ActiveProfile, name) {
			cfg.ActiveProfile = ""
		}
		if err := config.Save(cfg); err != nil {
			fmt.Printf("删除 profile 失败: %v\n", err)
			return
		}
		fmt.Printf("profile 已删除: %s\n", name)
	case "show":
		if cfg.Profiles == nil {
			fmt.Printf("profile %s 不存在\n", name)
			return
		}
		prof, ok := cfg.Profiles[name]
		if !ok {
			fmt.Printf("profile %s 不存在\n", name)
			return
		}
		b, err := json.MarshalIndent(prof, "", "  ")
		if err != nil {
			fmt.Printf("显示 profile 失败: %v\n", err)
			return
		}
		fmt.Println(string(b))
	default:
		fmt.Println("用法: /profile list | /profile switch <name> | /profile save <name> | /profile delete <name> | /profile show <name>")
	}
}

func handleRecordCommand(input string, eng *engine.Engine) {
	args := strings.Fields(strings.TrimSpace(strings.TrimPrefix(input, "/record")))
	if len(args) == 0 || strings.EqualFold(args[0], "status") {
		enabled, dir := eng.RecordingStatus()
		if enabled {
			fmt.Printf("recording: on (%s)\n", dir)
		} else {
			fmt.Printf("recording: off (%s)\n", dir)
		}
		return
	}
	if strings.EqualFold(args[0], "start") {
		dir := "./recordings"
		if len(args) > 1 {
			dir = args[1]
		}
		if err := eng.EnableRecording(dir); err != nil {
			fmt.Printf("启动录制失败: %v\n", err)
			return
		}
		fmt.Printf("recording 已开启: %s\n", dir)
		return
	}
	if strings.EqualFold(args[0], "stop") {
		eng.DisableRecording()
		fmt.Println("recording 已关闭")
		return
	}
	fmt.Println("用法: /record status | /record start <dir> | /record stop")
}
