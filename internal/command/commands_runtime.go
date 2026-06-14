package command

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func (c *DoctorCmd) Name() string        { return "doctor" }
func (c *DoctorCmd) Aliases() []string   { return nil }
func (c *DoctorCmd) Description() string { return "系统诊断" }
func (c *DoctorCmd) Help() string        { return "/doctor - 检查 Go、git、ripgrep、配置" }
func (c *DoctorCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	sb.WriteString("=== 系统诊断 ===\n")
	sb.WriteString(fmt.Sprintf("目录: %s\n", in.Cwd))
	if g, err := exec.LookPath("git"); err == nil {
		sb.WriteString(fmt.Sprintf("Git: %s\n", g))
	} else {
		sb.WriteString("Git: 未找到\n")
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		sb.WriteString(fmt.Sprintf("Ripgrep: %s\n", rg))
	} else {
		sb.WriteString("Ripgrep: 未找到\n")
	}
	sb.WriteString(fmt.Sprintf("时间: %s\n", time.Now().Format(time.RFC3339)))
	return Output{Message: sb.String()}, nil
}

func (c *ConfigCmd) Name() string        { return "config" }
func (c *ConfigCmd) Aliases() []string   { return nil }
func (c *ConfigCmd) Description() string { return "查看或修改配置" }
func (c *ConfigCmd) Help() string        { return "/config [键] [值] - 查看/设置配置" }
func (c *ConfigCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cfg := in.Config
	if cfg == nil {
		return Output{Message: "配置不可用"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "show" {
		return Output{Message: renderConfig(cfg)}, nil
	}
	key := strings.ToLower(in.Args[0])
	if len(in.Args) == 1 {
		return Output{Message: fmt.Sprintf("%s = %s", key, configValue(cfg, key))}, nil
	}
	value := strings.Join(in.Args[1:], " ")
	if err := applyConfigValue(cfg, key, value); err != nil {
		return Output{}, err
	}
	if in.SaveConfig != nil {
		if err := in.SaveConfig(cfg); err != nil {
			return Output{}, err
		}
	}
	return Output{Message: fmt.Sprintf("已保存 %s = %s", key, configValue(cfg, key))}, nil
}
