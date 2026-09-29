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

// skillInvocationPrompt turns "/<skill> [args]" into the user message that
// runs the skill: its instructions, with the arguments filled in by
// command.ExpandArguments (in place of $ARGUMENTS, or as the task after
// them). ok is false when no installed skill has that name.
//
// "/<skill> args" used to print the skill's text and "无效的参数" and run
// nothing: invoking a skill by name did not invoke it.
func skillInvocationPrompt(input string, eng *engine.Engine) (prompt, name string, ok bool) {
	parts := strings.Fields(input)
	if len(parts) == 0 || eng == nil || eng.Runtime() == nil {
		return "", "", false
	}
	name = strings.TrimPrefix(parts[0], "/")
	body, ok := eng.Runtime().SkillPrompts[name]
	if !ok {
		return "", name, false
	}
	args := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))
	expanded := command.ExpandArguments(body, args)
	prompt = fmt.Sprintf("Use the skill %q for this request. Its instructions:\n<skill name=%q>\n%s\n</skill>", name, name, strings.TrimSpace(expanded))
	if args == "" {
		prompt += "\n\nApply the skill to the current conversation and project."
	}
	return prompt, name, true
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
