package permission

import (
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safety"
)

// ExplainUncovered says why the allow rules remembered for toolName do not
// cover this call, for the prompt to show under its box: "asked again" with
// no reason looked like the rules were fake. It returns "" when nothing is
// remembered for the tool (and nothing for its sibling shell tool), or when
// the line is in fact covered.
func (m *Manager) ExplainUncovered(toolName string, input map[string]any) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var remembered []string
	cov := coverage{kind: shellKindFor(toolName, m.shellKind)}
	var siblingHas string
	command := inputCommand(input)
	programs := linePrograms(command)
	for _, r := range m.allow {
		if r.CommandPrefix == "" && r.CommandGroup == "" {
			continue
		}
		// Only a rule for a program this line runs explains anything. Every
		// remembered rule of the tool used to be named, so a "sed … > f"
		// line was said to fall outside a rule for docref.exe — a program
		// the task had nothing to do with.
		relevant := ruleConcerns(r, programs)
		if !toolMatches(r, toolName) {
			if relevant && IsShellTool(r.ToolPattern) && IsShellTool(toolName) {
				siblingHas = r.ToolPattern
			}
			continue
		}
		// Coverage is judged against every rule (another program's rule
		// may cover part of the line); only relevant ones are named.
		if r.CommandPrefix != "" {
			cov.prefixes = append(cov.prefixes, strings.Fields(r.CommandPrefix))
			if relevant {
				remembered = append(remembered, `"`+r.CommandPrefix+`"`)
			}
		} else {
			cov.groups = append(cov.groups, r.CommandGroup)
			if relevant {
				remembered = append(remembered, GroupLabel(r.CommandGroup))
			}
		}
	}
	if len(remembered) == 0 {
		if siblingHas != "" {
			return "已记住的规则属于 " + siblingHas + " 工具，这次模型用的是 " + toolName + " 工具，两者的规则不通用"
		}
		// No remembered rule concerns this line, so there is no gap to
		// explain. What still helps is why the line cannot be remembered
		// at all (the prompt then offers no [a]).
		if p := coverabilityProblem(command, cov.kind); p != "" {
			return "这一行无法按规则记住：" + p + "。按 y 允许本次，n 拒绝"
		}
		return ""
	}
	if lineCovered(command, cov) {
		return ""
	}
	reason := coverabilityProblem(command, cov.kind)
	if reason == "" {
		reason = uncoveredCommandReason(command, cov)
	}
	if reason == "" {
		reason = "这一行不在已记住的范围内"
	}
	return "上次记住的规则（" + strings.Join(remembered, "、") + "）未覆盖这一行：" + reason + "。按 y 允许本次，n 拒绝"
}

// linePrograms is the set of programs (programName) the simple commands of
// command run, including the one a runner runs ("sudo mkdir" runs mkdir).
func linePrograms(command string) map[string]bool {
	out := map[string]bool{}
	for _, c := range safety.SimpleCommands(command) {
		if len(c.Words) == 0 {
			continue
		}
		p := programName(c.Words[0])
		out[p] = true
		if commandRunners[p] {
			for _, w := range c.Words[1:] {
				if !strings.HasPrefix(w, "-") {
					out[programName(w)] = true
					break
				}
			}
		}
	}
	return out
}

// ruleConcerns reports whether rule r is about one of programs: its prefix
// starts with one of them, or its group holds one.
func ruleConcerns(r Rule, programs map[string]bool) bool {
	if f := strings.Fields(r.CommandPrefix); len(f) > 0 {
		return programs[programName(f[0])]
	}
	for p := range programs {
		if r.CommandGroup != "" && groupForProgram(p) == r.CommandGroup {
			return true
		}
	}
	return false
}

// coverabilityProblem names what keeps a line from being covered by any
// prefix or group rule (see commandCovered), or "" when the line itself is
// fine and only its commands are unremembered.
func coverabilityProblem(command string, kind ShellKind) string {
	if hasSubstitution(command) {
		return "本行含命令替换（$(…)、反引号或 ${…}），它们会执行别的命令，前缀和分组规则都不覆盖"
	}
	if maybePowerShell(kind) && (hasUnquotedBrace(command) || hasTypographicQuote(command)) {
		return "本行含 PowerShell 脚本块或弯引号，无法安全判定"
	}
	trustQuotes := quotingTrusted(command, kind)
	for _, c := range safety.SimpleCommands(command) {
		if len(c.Words) == 0 {
			continue
		}
		for _, r := range c.Redirects {
			if !discardTarget(r) {
				return "本行把输出重定向到文件（" + r + "），写文件不属于被允许的命令本身"
			}
		}
		if !literalWords(c, kind, trustQuotes) {
			if kind == ShellCmd || kind == "" {
				return "参数里有引号内的操作符（; & | < >），在 cmd.exe 下引号不受信任"
			}
			return "参数里有未加引号的操作符（; & | < > 或括号），无法确定实际会运行什么"
		}
		if commandRunners[programName(c.Words[0])] {
			return "`" + c.Words[0] + "` 会去运行别的命令，不能按前缀放行"
		}
	}
	return ""
}

// uncoveredCommandReason names the first command of the line that no
// remembered rule covers: a routine group's program with non-routine
// options, or a command outside the remembered set.
func uncoveredCommandReason(command string, cov coverage) string {
	cmds, ok := coverableCommands(command, cov.kind)
	if !ok {
		return ""
	}
	for _, words := range cmds {
		if cov.covers(words) {
			continue
		}
		shown := strings.Join(words, " ")
		if len(words) > 4 {
			shown = strings.Join(words[:4], " ") + " …"
		}
		if id := groupForProgram(words[0]); id != "" && plainProgram(words[0]) {
			for _, g := range cov.groups {
				if g == id {
					return "命令 `" + shown + "` 属于 " + GroupLabel(id) + "，但带有不在常规范围内的子命令或参数"
				}
			}
		}
		return "命令 `" + shown + "` 不在已记住的范围内"
	}
	return ""
}
