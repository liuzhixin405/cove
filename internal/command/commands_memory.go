package command

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func (c *MemoryCmd) Name() string        { return "memory" }
func (c *MemoryCmd) Aliases() []string   { return nil }
func (c *MemoryCmd) Description() string { return "管理持久化记忆" }
func (c *MemoryCmd) Help() string {
	return "/memory [list|add|remove|search <关键词>|stats] - 管理持久记忆文件"
}
func (c *MemoryCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.MemoryStore == nil {
		return Output{Message: "记忆存储不可用"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "list" {
		entries := in.MemoryStore.All()
		if len(entries) == 0 {
			return Output{Message: "暂无记忆文件"}, nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		var sb strings.Builder
		sb.WriteString("记忆文件:\n")
		for _, e := range entries {
			sb.WriteString(fmt.Sprintf("- %s\n", e.Name))
		}
		return Output{Message: sb.String()}, nil
	}
	switch in.Args[0] {
	case "add":
		if len(in.Args) < 3 {
			return Output{Message: "用法: /memory add <名称> <内容>"}, nil
		}
		name := in.Args[1]
		content := strings.Join(in.Args[2:], " ")
		if err := in.MemoryStore.Save(name, content); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("记忆 '%s' 已保存", name)}, nil
	case "remove", "delete", "rm":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /memory remove <名称>"}, nil
		}
		if err := in.MemoryStore.Delete(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("记忆 '%s' 已删除", in.Args[1])}, nil
	case "search", "find":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /memory search <关键词>"}, nil
		}
		query := strings.Join(in.Args[1:], " ")
		results := in.MemoryStore.Search(query, 5)
		if len(results) == 0 {
			return Output{Message: fmt.Sprintf("未找到与 %q 相关的记忆", query)}, nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("与 %q 相关的记忆 (BM25 关键词检索):\n", query))
		for _, r := range results {
			marker := ""
			if r.Entry.Project {
				marker = " (项目)"
			}
			preview := strings.ReplaceAll(r.Entry.Content, "\n", " ")
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			sb.WriteString(fmt.Sprintf("  %s%s [%.2f]: %s\n", r.Entry.Name, marker, r.Score, preview))
		}
		return Output{Message: sb.String()}, nil
	case "stats", "stat":
		st := in.MemoryStore.Stats()
		var sb strings.Builder
		sb.WriteString("记忆统计:\n")
		sb.WriteString(fmt.Sprintf("  文件数:   %d (其中项目记忆 %d)\n", st.FileCount, st.ProjectCount))
		sb.WriteString(fmt.Sprintf("  总行数:   %d\n", st.TotalLines))
		sb.WriteString(fmt.Sprintf("  总大小:   %s / %s\n", humanBytes(st.TotalBytes), humanBytes(st.MaxTotalBytes)))
		if st.MaxTotalBytes > 0 {
			sb.WriteString(fmt.Sprintf("  使用率:   %.1f%%\n", float64(st.TotalBytes)*100/float64(st.MaxTotalBytes)))
		}
		sb.WriteString(fmt.Sprintf("  单条上限: %s\n", humanBytes(st.MaxEntryBytes)))
		return Output{Message: sb.String()}, nil
	default:
		return Output{Message: "用法: /memory [list|add|remove|search <关键词>|stats]"}, nil
	}
}
