package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove/internal/skills"
)

// --- skills_list ---

type SkillsListTool struct{ baseTool }

func NewSkillsListTool() Tool {
	return &SkillsListTool{baseTool{def: Def{
		Name:        "skills_list",
		Description: "List available skills (name + description). Use skill_view(name) to load full content.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		IsReadOnly:  true,
	}}}
}

func (t *SkillsListTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	sm := getSkillManager(tctx)
	if sm == nil {
		return Result{Data: "No skills available", IsError: false}, nil
	}

	all := sm.All()
	if len(all) == 0 {
		return Result{Data: "No skills installed. Use /skill create <name> to create one.", IsError: false}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d skills available:\n\n", len(all)))
	for _, s := range all {
		desc := s.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		sb.WriteString(fmt.Sprintf("  %-20s %s\n", s.Name, desc))
	}
	return Result{Data: sb.String(), IsError: false}, nil
}

func (t *SkillsListTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return PermissionDecision{Decision: Allow}
}

// --- skill_view ---

type SkillViewTool struct{ baseTool }

func NewSkillViewTool() Tool {
	return &SkillViewTool{baseTool{def: Def{
		Name:        "skill_view",
		Description: "Load a skill's full content by name. Use skills_list first to see available skills. Returns the complete SKILL.md content.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"name":{"type":"string","description":"Skill name (use skills_list to see available skills)"}
			},
			"required":["name"]
		}`),
		IsReadOnly: true,
	}}}
}

func (t *SkillViewTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	if name == "" {
		return Result{Data: "Error: skill name is required", IsError: true}, nil
	}

	sm := getSkillManager(tctx)
	if sm == nil {
		return Result{Data: "No skills available", IsError: false}, nil
	}

	skill, ok := sm.Get(name)
	if !ok {
		all := sm.All()
		var suggestions []string
		for _, s := range all {
			if strings.Contains(strings.ToLower(s.Name), strings.ToLower(name)) ||
				strings.Contains(strings.ToLower(name), strings.ToLower(s.Name)) {
				suggestions = append(suggestions, s.Name)
			}
		}
		msg := fmt.Sprintf("Skill '%s' not found.", name)
		if len(suggestions) > 0 {
			msg += fmt.Sprintf(" Did you mean: %s?", strings.Join(suggestions, ", "))
		}
		msg += " Use skills_list to see all available skills."
		return Result{Data: msg, IsError: true}, nil
	}

	return Result{Data: fmt.Sprintf("[Skill: %s]\n\n%s", skill.Name, skill.Prompt), IsError: false}, nil
}

func (t *SkillViewTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return PermissionDecision{Decision: Allow}
}

// --- helpers ---

func getSkillManager(tctx Context) *skills.Manager {
	if tctx.Runtime == nil || tctx.Runtime.SkillManager == nil {
		return nil
	}
	sm, ok := tctx.Runtime.SkillManager.(*skills.Manager)
	if !ok {
		return nil
	}
	return sm
}
