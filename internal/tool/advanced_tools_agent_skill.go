package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/skills"
)

type SkillTool struct{ baseTool }
type AgentToolI struct{ baseTool }

func NewSkillTool() Tool {
	return &SkillTool{baseTool{def: Def{
		Name: "skill", Description: "Execute a skill (predefined workflow).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"args":{"type":"object"}},"required":["name"]}`),
		IsReadOnly:  false, UserFacingName: "Skill",
	}}}
}
func (t *SkillTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	if tctx.Runtime != nil {
		if tctx.Runtime.SkillPrompts != nil {
			if prompt, ok := tctx.Runtime.SkillPrompts[name]; ok {
				return Result{Data: fmt.Sprintf("[Skill: %s]\n\n%s\n\nFollow these instructions to complete the task.", name, prompt)}, nil
			}
		}
		if mgr, ok := tctx.Runtime.SkillManager.(interface {
			Get(string) (skills.Skill, bool)
		}); ok {
			if skill, found := mgr.Get(name); found {
				return Result{Data: fmt.Sprintf("[Skill: %s]\n\n%s\n\nFollow these instructions to complete the task.", name, skill.Prompt)}, nil
			}
		}
		if tctx.Runtime.SkillPrompts != nil {
			return Result{Data: fmt.Sprintf("Skill '%s' activated. Available skills: %v", name, tctx.Runtime.SkillPrompts)}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Skill '%s' activated. No runtime skill registry available.", name)}, nil
}
func (t *SkillTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("skill execution is safe")
}

func NewAgentTool() Tool {
	return &AgentToolI{baseTool{def: Def{
		Name: "agent", Aliases: []string{"Agent"},
		Description: "Spawn a sub-agent to handle complex multi-step tasks independently.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string","description":"Agent type: general, explore, plan, review, test"},"prompt":{"type":"string","description":"Task description for the sub-agent"}},"required":["type","prompt"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, UserFacingName: "Agent",
	}}}
}
func (t *AgentToolI) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	agentType, _ := input["type"].(string)
	task, _ := input["prompt"].(string)
	if tctx.Runtime != nil && tctx.Runtime.AgentRunner != nil {
		if runner, ok := tctx.Runtime.AgentRunner.(api.AgentRunner); ok {
			result, err := runner.Run(ctx, agentType, task)
			if err != nil {
				return Result{Data: fmt.Sprintf("Sub-agent error: %v", err), IsError: true}, nil
			}
			return Result{Data: fmt.Sprintf("Sub-agent [%s] result:\n%s\nCost: $%.4f | Steps: %d | Success: %v",
				agentType, result.Output, result.Cost, result.Steps, result.Success)}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Sub-agent runner unavailable. Requested [%s]: %s", agentType, truncateStr(task, 300)), IsError: true}, nil
}
func (t *AgentToolI) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("agent spawning is safe")
}
