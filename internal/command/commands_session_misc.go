package command

import (
	"context"
	"fmt"
	"strings"
)

func (c *CompactCmd) Name() string        { return "compact" }
func (c *CompactCmd) Aliases() []string   { return nil }
func (c *CompactCmd) Description() string { return "压缩对话历史" }
func (c *CompactCmd) Help() string {
	return "/compact - 总结早期消息以释放上下文窗口"
}
func (c *CompactCmd) Execute(ctx context.Context, in Input) (Output, error) {
	return Output{Message: "请从 REPL 内置路径使用 /compact。"}, nil
}

func (c *CostCmd) Name() string        { return "cost" }
func (c *CostCmd) Aliases() []string   { return nil }
func (c *CostCmd) Description() string { return "查看用量和费用" }
func (c *CostCmd) Help() string        { return "/cost - 显示会话 token 用量和预估费用" }
func (c *CostCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "费用跟踪器不可用"}, nil
	}
	return Output{Message: in.Engine.CostTracker().Summary()}, nil
}

func (c *ResumeCmd) Name() string        { return "resume" }
func (c *ResumeCmd) Aliases() []string   { return nil }
func (c *ResumeCmd) Description() string { return "恢复已保存的会话" }
func (c *ResumeCmd) Help() string        { return "/resume [session-id] - 列出或恢复已保存的会话" }
func (c *ResumeCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SessionStore == nil {
		return Output{Message: "会话存储不可用"}, nil
	}
	if len(in.Args) == 0 {
		records, err := in.SessionStore.List()
		if err != nil {
			return Output{}, err
		}
		if len(records) == 0 {
			return Output{Message: "暂无已保存的会话"}, nil
		}
		var sb strings.Builder
		sb.WriteString("已保存的会话:\n")
		for _, r := range records {
			sb.WriteString(fmt.Sprintf("- %s  %s  (%d tokens)\n", r.ID, r.Title, r.TokensIn+r.TokensOut))
		}
		return Output{Message: sb.String()}, nil
	}
	r, err := in.SessionStore.Load(in.Args[0])
	if err != nil {
		return Output{}, err
	}
	if in.Engine != nil {
		in.Engine.LoadMessages(r.Messages)
	}
	if in.AppState != nil {
		in.AppState.SessionID = r.ID
		if r.Model != "" {
			in.AppState.Model = r.Model
		}
		in.AppState.Messages = len(r.Messages)
		in.AppState.BudgetUsed = r.Cost
	}
	return Output{Message: fmt.Sprintf("已恢复: %s (%d 条消息, %d tokens)", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)}, nil
}

func (c *HistoryCmd) Name() string        { return "history" }
func (c *HistoryCmd) Aliases() []string   { return nil }
func (c *HistoryCmd) Description() string { return "查看或选回历史会话" }
func (c *HistoryCmd) Help() string {
	return "/history - 获取详细的交互式历史会话菜单列表"
}
func (c *HistoryCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SessionStore == nil {
		return Output{Message: "会话存储不可用"}, nil
	}
	records, err := in.SessionStore.List()
	if err != nil {
		return Output{}, err
	}
	if len(records) == 0 {
		return Output{Message: "暂无已保存的会话"}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("历史记录 (%d 个会话):\n\n", len(records)))
	limit := 20
	if len(records) < limit {
		limit = len(records)
	}
	for i, r := range records[:limit] {
		msgCount := r.MessageCount
		if msgCount == 0 && len(r.Messages) > 0 {
			msgCount = len(r.Messages)
		}
		title := r.Title
		if title == "New session" || title == "" {
			// Use preview from first user message
			preview := ""
			for _, m := range r.Messages {
				if m.Role == "user" && m.Content != "" {
					content := strings.ReplaceAll(m.Content, "\n", " ")
					if len(content) > 50 {
						content = content[:50] + "..."
					}
					preview = content
					break
				}
			}
			if preview != "" {
				title = preview
			}
		}
		if len(title) > 50 {
			title = title[:50] + "..."
		}
		sb.WriteString(fmt.Sprintf("  %d. [%s] %s  (%d 条)\n", i+1, r.UpdatedAt.Format("01-02 15:04"), title, msgCount))
	}
	if len(records) > limit {
		sb.WriteString(fmt.Sprintf("\n  ... 还有 %d 条。使用 Ctrl+R 查看全部。\n", len(records)-limit))
	}
	sb.WriteString("\n提示: 在 TUI 界面中使用 Ctrl+R 可唤起选择浮层直接选回历史会话。\n")
	return Output{Message: sb.String()}, nil
}
