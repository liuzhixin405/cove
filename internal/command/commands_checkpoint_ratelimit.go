package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

type checkpointEngine interface {
	ListCheckpoints() []string
	RestoreCheckpoint(commitHash string) error
}

type rateLimitEngine interface {
	RateLimitInfo() api.RateLimitInfo
}

func (c *UndoCmd) Name() string        { return "undo" }
func (c *UndoCmd) Aliases() []string   { return nil }
func (c *UndoCmd) Description() string { return "回退到检查点" }
func (c *UndoCmd) Help() string        { return "/undo [commit] - 回退到最近或指定检查点" }
func (c *UndoCmd) Execute(ctx context.Context, in Input) (Output, error) {
	eng, ok := in.Engine.(checkpointEngine)
	if !ok || eng == nil {
		return Output{Message: "检查点功能不可用"}, nil
	}
	hash := ""
	if len(in.Args) > 0 {
		hash = strings.TrimSpace(in.Args[0])
	}
	if err := eng.RestoreCheckpoint(hash); err != nil {
		return Output{Message: fmt.Sprintf("回退失败: %v", err)}, nil
	}
	if hash == "" {
		return Output{Message: "已回退到最近检查点"}, nil
	}
	return Output{Message: fmt.Sprintf("已回退到检查点 %s", hash)}, nil
}

func (c *CheckpointsCmd) Name() string        { return "checkpoints" }
func (c *CheckpointsCmd) Aliases() []string   { return nil }
func (c *CheckpointsCmd) Description() string { return "列出可用检查点" }
func (c *CheckpointsCmd) Help() string        { return "/checkpoints - 列出最近检查点" }
func (c *CheckpointsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	eng, ok := in.Engine.(checkpointEngine)
	if !ok || eng == nil {
		return Output{Message: "检查点功能不可用"}, nil
	}
	items := eng.ListCheckpoints()
	if len(items) == 0 {
		return Output{Message: "暂无检查点"}, nil
	}
	var sb strings.Builder
	sb.WriteString("最近检查点:\n")
	for i, it := range items {
		sb.WriteString(fmt.Sprintf("%2d. %s\n", i+1, it))
	}
	return Output{Message: strings.TrimRight(sb.String(), "\n")}, nil
}

func (c *RateLimitCmd) Name() string        { return "ratelimit" }
func (c *RateLimitCmd) Aliases() []string   { return nil }
func (c *RateLimitCmd) Description() string { return "查看 API 速率限制状态" }
func (c *RateLimitCmd) Help() string {
	return "/ratelimit - 查看最近一次请求的速率限制信息"
}
func (c *RateLimitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	eng, ok := in.Engine.(rateLimitEngine)
	if !ok || eng == nil {
		return Output{Message: "速率限制信息不可用"}, nil
	}
	info := eng.RateLimitInfo()
	if !info.HasData() {
		return Output{Message: "暂无速率限制数据（尚未收到相关响应头）"}, nil
	}
	var sb strings.Builder
	sb.WriteString("=== Rate Limit ===\n")
	if info.RequestsLimit > 0 {
		sb.WriteString(fmt.Sprintf("Requests: %d / %d", info.RequestsRemaining, info.RequestsLimit))
		if info.RequestsReset > 0 {
			sb.WriteString(fmt.Sprintf(" (reset in %s)", roundDuration(info.RequestsReset)))
		}
		sb.WriteString("\n")
	}
	if info.TokensLimit > 0 {
		sb.WriteString(fmt.Sprintf("Tokens: %d / %d", info.TokensRemaining, info.TokensLimit))
		if info.TokensReset > 0 {
			sb.WriteString(fmt.Sprintf(" (reset in %s)", roundDuration(info.TokensReset)))
		}
		sb.WriteString("\n")
	}
	if !info.UpdatedAt.IsZero() {
		sb.WriteString(fmt.Sprintf("Updated: %s", info.UpdatedAt.Format(time.RFC3339)))
	}
	return Output{Message: strings.TrimRight(sb.String(), "\n")}, nil
}

func roundDuration(d time.Duration) time.Duration {
	if d < time.Second {
		return d
	}
	return d.Round(time.Second)
}
