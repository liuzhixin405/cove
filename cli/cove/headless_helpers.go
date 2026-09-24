package main

import (
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/plugin"
)

// Helpers the headless front end shares with the interactive shell.
//
// They return rendered strings instead of printing, because the headless path
// decides for itself where its output goes. The rest of this file went away
// with the full-screen shell that needed it.

// skillInvocationRequested reports whether input is a bare "/<skillname>" that maps
// to an installed skill (not /skill, /skills, or a registered command).
func skillInvocationRequested(input string, eng *engine.Engine) bool {
	if eng == nil || eng.Runtime() == nil {
		return false
	}
	parts := strings.Fields(input)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	name := strings.TrimPrefix(parts[0], "/")
	if name == "" || name == "skill" || name == "skills" {
		return false
	}
	_, ok := eng.Runtime().SkillPrompts[name]
	return ok
}

// skillInvocationText renders an installed skill's prompt for display,
// mirroring the classic REPL's handleSkillInvocation.
func skillInvocationText(input string, eng *engine.Engine) string {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return ""
	}
	name := strings.TrimPrefix(parts[0], "/")
	prompt, ok := eng.Runtime().SkillPrompts[name]
	if !ok {
		return fmt.Sprintf("未找到配置文件: %s", name)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[Skill: %s]\n\n%s\n", name, prompt)
	if args := strings.TrimSpace(strings.TrimPrefix(input, parts[0])); args != "" {
		fmt.Fprintf(&sb, "\n无效的参数: %s\n", args)
	}
	return sb.String()
}

// pluginCommandPrompt resolves a "/<plugincmd> [args]" into the plugin
// command's prompt body, with the args filled in by command.ExpandArguments.
// ok is false when the command does not match an enabled plugin command. The
// caller feeds the returned prompt to the engine as a normal user turn.
func pluginCommandPrompt(input string, pluginMgr *plugin.Manager) (prompt string, label string, ok bool) {
	if pluginMgr == nil {
		return "", "", false
	}
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", "", false
	}
	name := strings.TrimPrefix(parts[0], "/")
	cmd, found := pluginMgr.CommandPrompts()[name]
	if !found {
		return "", "", false
	}
	p := command.ExpandArguments(cmd.Prompt, strings.TrimPrefix(input, parts[0]))
	return p, fmt.Sprintf("%s (%s)", name, cmd.Plugin), true
}
