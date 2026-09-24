package tool

import (
	"context"
	"strings"
	"testing"
)

// An unknown skill name was answered with "Skill 'x' activated" plus the full
// prompt text of every registered skill (the map was printed with %v), and no
// error flag: the model believed a skill ran that does not exist.
func TestSkillToolUnknownNameIsAnErrorListingNamesOnly(t *testing.T) {
	rt := &Runtime{SkillPrompts: map[string]string{
		"deploy": "SECRET-LONG-DEPLOY-PROMPT-BODY",
		"review": "SECRET-LONG-REVIEW-PROMPT-BODY",
	}}
	res, _ := NewSkillTool().Call(context.Background(), Input{"name": "deplyo"}, Context{Runtime: rt})
	if !res.IsError {
		t.Fatalf("unknown skill reported as success: %q", res.Data)
	}
	if strings.Contains(res.Data, "activated") {
		t.Errorf("unknown skill claims to be activated: %q", res.Data)
	}
	if strings.Contains(res.Data, "PROMPT-BODY") {
		t.Errorf("result dumps skill prompt bodies: %q", res.Data)
	}
	for _, name := range []string{"deploy", "review"} {
		if !strings.Contains(res.Data, name) {
			t.Errorf("result does not list available skill %q: %q", name, res.Data)
		}
	}
}

func TestSkillToolWithoutRegistryIsAnError(t *testing.T) {
	res, _ := NewSkillTool().Call(context.Background(), Input{"name": "x"}, Context{})
	if !res.IsError || strings.Contains(res.Data, "activated") {
		t.Fatalf("want an error when no skill registry exists, got IsError=%v %q", res.IsError, res.Data)
	}
}

func TestSkillToolKnownPromptStillRenders(t *testing.T) {
	rt := &Runtime{SkillPrompts: map[string]string{"deploy": "run the deploy script"}}
	res, _ := NewSkillTool().Call(context.Background(), Input{"name": "deploy"}, Context{Runtime: rt})
	if res.IsError || !strings.Contains(res.Data, "run the deploy script") {
		t.Fatalf("known skill: IsError=%v %q", res.IsError, res.Data)
	}
}
