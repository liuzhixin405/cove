package command

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/textutil"
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
		fmt.Fprintf(&sb, "记忆文件 (共 %d 条):\n", len(entries))
		for _, e := range entries {
			marker := sourceMarker(e)
			fmt.Fprintf(&sb, "- %s%s [%s]: %s\n", e.Name, marker, humanBytes(len(e.Content)), entrySummary(e.Content))
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
		// A name only the global directory has: build on its content, or the
		// new project copy would hide what the global one said.
		if ap, ok := in.MemoryStore.(memoryAppender); ok {
			if _, fromLower, exists := ap.BaseContent(name); exists && fromLower {
				written, err := ap.Append(name, content)
				if err != nil {
					return Output{}, err
				}
				return Output{Message: fmt.Sprintf("记忆 '%s' 已保存（在全局同名记忆基础上追加到项目目录）", written)}, nil
			}
		}
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
		fmt.Fprintf(&sb, "与 %q 相关的记忆 (BM25 关键词检索):\n", query)
		for _, r := range results {
			marker := sourceMarker(r.Entry)
			preview := strings.ReplaceAll(r.Entry.Content, "\n", " ")
			if len(preview) > 80 {
				preview = textutil.ClipRunes(preview, 83)
			}
			fmt.Fprintf(&sb, "  %s%s [%.2f]: %s\n", r.Entry.Name, marker, r.Score, preview)
		}
		return Output{Message: sb.String()}, nil
	case "stats", "stat":
		st := in.MemoryStore.Stats()
		var sb strings.Builder
		sb.WriteString("记忆统计:\n")
		fmt.Fprintf(&sb, "  文件数:   %d (其中指令文件 %d)\n", st.FileCount, st.ProjectCount)
		fmt.Fprintf(&sb, "  总行数:   %d\n", st.TotalLines)
		fmt.Fprintf(&sb, "  总大小:   %s / %s\n", humanBytes(st.TotalBytes), humanBytes(st.MaxTotalBytes))
		if st.MaxTotalBytes > 0 {
			fmt.Fprintf(&sb, "  使用率:   %.1f%%\n", float64(st.TotalBytes)*100/float64(st.MaxTotalBytes))
		}
		fmt.Fprintf(&sb, "  单条上限: %s\n", humanBytes(st.MaxEntryBytes))
		if st.LastExtractedAt.IsZero() {
			sb.WriteString("  上次提取: 尚无记录\n")
		} else {
			fmt.Fprintf(&sb, "  上次提取: %s，保存 %d 条\n", st.LastExtractedAt.Format("2006-01-02 15:04"), st.LastExtractedCount)
		}
		return Output{Message: sb.String()}, nil
	default:
		return Output{Message: "用法: /memory [list|add|remove|search <关键词>|stats]"}, nil
	}
}

// memoryAppender is the part of *memory.Store /memory add uses for a name
// that only a lower-priority (global) directory has.
type memoryAppender interface {
	BaseContent(name string) (content string, fromLower, ok bool)
	Append(name, content string) (string, error)
}

// sourceMarker labels where a memory entry was loaded from.
func sourceMarker(e memory.Entry) string {
	switch {
	case e.Project || e.Source == memory.SourceInstructions:
		return " (指令文件)"
	case e.Source == memory.SourceProject:
		return " (项目)"
	case e.Source == memory.SourceGlobal:
		return " (全局)"
	}
	return ""
}

// entrySummary is the first non-blank line of a memory, clipped for a list.
func entrySummary(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return textutil.ClipRunes(line, 60)
		}
	}
	return "(空)"
}
