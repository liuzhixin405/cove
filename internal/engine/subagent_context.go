package engine

import (
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/repomap"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// subAgentContext is the project context every sub-agent (the agent tool and
// execute_plan) gets after its role prompt: where it runs, the project's
// shape, the project instruction files and the user's custom instructions.
// Sub-agents used to start from a one-line role alone, so they ignored the
// project's rules (CLAUDE.md/AGENTS.md) and guessed at its layout and shell.
//
// Saved memories are left out: they are about the user and the conversation,
// which the parent handles; the sub-agent reports back to the parent.
func (e *Engine) subAgentContext() string {
	var parts []string
	if pc := e.projCtx; pc != nil {
		env := fmt.Sprintf("Working directory: %s | Platform: %s | Shell: %s", pc.Cwd, pc.Platform, pc.Shell)
		if pc.IsGitRepo {
			if branch, _ := pc.GetGitInfo(); branch != "" {
				env += " | Git branch: " + branch
			}
		}
		parts = append(parts, env)
		if outline := repomap.Outline(e.repoMapRoot()); outline != "" {
			parts = append(parts, strings.TrimSpace(outline))
		}
	}
	if e.memStore != nil {
		if instr := e.memStore.InstructionsPrompt(); instr != "" {
			parts = append(parts, "Follow the project's instructions:\n"+strings.TrimSpace(instr))
		}
	}
	if ci := strings.TrimSpace(e.config.CustomInstructions); ci != "" {
		parts = append(parts, "# User Instructions\n\n"+ci)
	}
	if len(parts) == 0 {
		return ""
	}
	out := "# Project context\n\n" + strings.Join(parts, "\n\n")
	// The same budget the parent's optional context gets (about 3 bytes a
	// token): on a small window the instructions must not crowd out the task.
	return textutil.ClipBytes(out, api.StaticContextBudget(e.config.Model)*3, "\n... [project context truncated]")
}

// subAgentContextBudget is the token budget of one sub-agent request on
// model: the parent's compaction trigger, so a sub-agent trims its old tool
// results at the point the parent would compact.
func subAgentContextBudget(model string) int {
	return compactionThreshold(model)
}
