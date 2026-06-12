package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/tool"
)

type cmdEntry struct {
	Name     string
	Desc     string
	Type     string
	Args     []string
	ArgHints map[string][]string
}

func buildCommandList(cmdReg *command.Registry, toolReg *tool.Registry) []cmdEntry {
	var list []cmdEntry
	for _, c := range cmdReg.All() {
		list = append(list, cmdEntry{Name: "/" + c.Name(), Desc: c.Description(), Type: "cmd"})
	}
	list = append(list,
		cmdEntry{Name: "/model", Desc: "设置模型", Type: "config"},
		cmdEntry{Name: "/provider", Desc: "设置供应商", Type: "config", ArgHints: map[string][]string{"": providerNameSuggestions()}},
		cmdEntry{Name: "/api-key", Desc: "设置 API 密钥", Type: "config"},
		cmdEntry{Name: "/base-url", Desc: "设置 API 地址", Type: "config"},
		cmdEntry{Name: "/mode", Desc: "设置权限模式 (default|plan|auto|bypass)", Type: "config", ArgHints: map[string][]string{"": {"default", "plan", "auto", "bypass"}}},
		cmdEntry{Name: "/budget", Desc: "设置预算上限 ($)", Type: "config"},
		cmdEntry{Name: "/attach", Desc: "挂载图片或文件到后续提问", Type: "builtin", ArgHints: map[string][]string{"": {"list", "clear", "remove", "add"}}},
		cmdEntry{Name: "/help", Desc: "显示帮助", Type: "builtin"},
		cmdEntry{Name: "/exit", Desc: "退出", Type: "builtin"},
		cmdEntry{Name: "/history", Desc: "查看和继续历史会话", Type: "builtin"},
		cmdEntry{Name: "/tasks", Desc: "查看运行中/排队的后台任务", Type: "builtin"},
		cmdEntry{Name: "/stop", Desc: "取消当前运行的任务", Type: "builtin"},
	)
	for _, t := range toolReg.All() {
		d := t.Def()
		args := toolArgNames(d.InputSchema)
		list = append(list, cmdEntry{Name: d.Name, Desc: d.Description, Type: "tool", Args: args})
		for _, alias := range d.Aliases {
			list = append(list, cmdEntry{Name: alias, Desc: d.Description, Type: "tool", Args: args})
		}
	}
	return list
}

func buildSkillDescs(mgr *skills.Manager) map[string]string {
	descs := make(map[string]string)
	if mgr == nil {
		return descs
	}
	for _, s := range mgr.All() {
		if s.Description != "" {
			descs[s.Name] = s.Description
		} else {
			descs[s.Name] = "技能"
		}
	}
	return descs
}

func complete(input string, commands []cmdEntry, skills map[string]string) []string {
	input = strings.TrimLeft(input, " \t")
	if input == "" {
		return nil
	}
	if suggestions := completeArgs(input, commands); len(suggestions) > 0 {
		return suggestions
	}
	cmdNames := make(map[string]bool, len(commands))
	var matches []string
	lower := strings.ToLower(input)
	for _, c := range commands {
		cmdNames[strings.ToLower(c.Name)] = true
		if strings.HasPrefix(input, "/") && c.Type == "tool" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(c.Name), lower) {
			if c.Desc != "" {
				matches = append(matches, c.Name+"\t"+shortDesc(c.Desc))
			} else {
				matches = append(matches, c.Name)
			}
		}
	}
	for name, desc := range skills {
		candidate := name
		if strings.HasPrefix(input, "/") {
			candidate = "/" + name
		}
		if cmdNames[strings.ToLower(candidate)] {
			continue
		}
		if strings.HasPrefix(strings.ToLower(candidate), lower) {
			if desc != "" {
				matches = append(matches, candidate+"\t"+shortDesc(desc))
			} else {
				matches = append(matches, candidate+"\t"+"技能")
			}
		}
	}
	sort.Strings(matches)
	return matches
}

func completeArgs(input string, commands []cmdEntry) []string {
	head, rest, ok := strings.Cut(input, " ")
	if !ok {
		return nil
	}
	entry, found := findCompletionEntry(head, commands)
	if !found {
		return nil
	}
	rest = strings.TrimLeft(rest, " \t")
	base := head + " "
	if hints := entry.ArgHints[""]; len(hints) > 0 {
		return completeValueHints(base, rest, hints)
	}
	if len(entry.Args) == 0 {
		return nil
	}
	used := usedArgNames(rest)
	current := currentArgPrefix(rest)
	var matches []string
	for _, arg := range entry.Args {
		if used[arg] {
			continue
		}
		if current == "" || strings.HasPrefix(strings.ToLower(arg), strings.ToLower(current)) {
			matches = append(matches, base+replaceCurrentArgPrefix(rest, current, arg+"="))
		}
	}
	return matches
}

func completeValueHints(base, rest string, hints []string) []string {
	current := strings.TrimSpace(rest)
	var matches []string
	for _, hint := range hints {
		if current == "" || strings.HasPrefix(strings.ToLower(hint), strings.ToLower(current)) {
			matches = append(matches, base+hint)
		}
	}
	return matches
}

func findCompletionEntry(name string, commands []cmdEntry) (cmdEntry, bool) {
	for _, c := range commands {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return cmdEntry{}, false
}

func usedArgNames(input string) map[string]bool {
	used := map[string]bool{}
	for _, part := range strings.Fields(input) {
		part = strings.Trim(part, ` "'{},`)
		if key, _, ok := strings.Cut(part, "="); ok {
			used[strings.Trim(key, ` "'`)] = true
			continue
		}
		if key, _, ok := strings.Cut(part, ":"); ok {
			used[strings.Trim(key, ` "'`)] = true
		}
	}
	return used
}

func currentArgPrefix(input string) string {
	input = strings.TrimRight(input, " \t")
	if input == "" {
		return ""
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	if strings.ContainsAny(last, "=:") {
		return ""
	}
	return strings.Trim(last, ` "'{},`)
}

func replaceCurrentArgPrefix(input, current, replacement string) string {
	if current == "" {
		if strings.TrimSpace(input) == "" {
			return replacement
		}
		return strings.TrimRight(input, " \t") + " " + replacement
	}
	idx := strings.LastIndex(input, current)
	if idx < 0 {
		return strings.TrimRight(input, " \t") + " " + replacement
	}
	return input[:idx] + replacement
}

func toolArgNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if required[names[i]] != required[names[j]] {
			return required[names[i]]
		}
		return names[i] < names[j]
	})
	return names
}

func providerNameSuggestions() []string {
	return []string{
		"anthropic", "deepseek", "openai", "openai-compatible", "glm", "kimi", "qwen", "doubao",
		"openrouter", "siliconflow", "groq", "together", "fireworks", "xai", "mistral",
	}
}

func showQuickCommands(commands []cmdEntry) {
	fmt.Println("\n可用命令:")
	for _, c := range commands {
		if c.Type == "cmd" || c.Type == "config" || c.Type == "builtin" {
			fmt.Printf("  %-16s %s\n", c.Name, c.Desc)
		}
	}
	fmt.Println()
}

func handleUnknownCmd(input string, cmdReg *command.Registry) bool {
	parts := strings.Fields(input)
	name := strings.TrimPrefix(parts[0], "/")
	_, ok := cmdReg.Find(name)
	if ok {
		return false
	}
	suggestions := fuzzyMatch(name, cmdReg)
	if len(suggestions) > 0 {
		fmt.Printf("未知命令: /%s\n你是不是想输入?\n", name)
		for _, s := range suggestions {
			fmt.Printf("  /%s\n", s)
		}
		return true
	}
	fmt.Printf("未知命令: /%s。输入 /help 查看可用命令。\n", name)
	return true
}

func fuzzyMatch(input string, cmdReg *command.Registry) []string {
	var matches []string
	lower := strings.ToLower(input)
	for _, c := range cmdReg.All() {
		name := c.Name()
		if strings.Contains(strings.ToLower(name), lower) {
			matches = append(matches, name)
		}
	}
	if len(matches) > 5 {
		return matches[:5]
	}
	return matches
}
