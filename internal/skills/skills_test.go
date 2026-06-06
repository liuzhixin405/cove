package skills

import "testing"

func TestInstallHTTPClientHasTimeout(t *testing.T) {
	if installHTTPClient.Timeout <= 0 {
		t.Fatal("InstallSkill HTTP client should have a timeout")
	}
}

func TestRegisterBundlesIncludesClaudeStyleBuiltins(t *testing.T) {
	mgr := NewManager()
	RegisterBundles(mgr)

	required := []string{
		"batch",
		"debug",
		"keybindings-help",
		"lorem-ipsum",
		"remember",
		"simplify",
		"skillify",
		"stuck",
		"update-config",
		"verify",
		"loop",
		"schedule",
		"claude-api",
		"claude-in-chrome",
	}
	for _, name := range required {
		skill, ok := mgr.Get(name)
		if !ok {
			t.Fatalf("expected bundled skill %q to be registered", name)
		}
		if skill.Prompt == "" || skill.Description == "" {
			t.Fatalf("bundled skill %q should include prompt and description: %#v", name, skill)
		}
	}
}
