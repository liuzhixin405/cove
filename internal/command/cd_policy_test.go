package command

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// policyEngine is a liveEngine whose policies.json failed to load.
type policyEngine struct {
	liveEngine
	err error
}

func (p *policyEngine) PolicyLoadError() error { return p.err }

// After /cd reloads policies.json for the new project, a file that cannot be
// parsed is reported: the previous project's rules stay in effect and the new
// project's are not applied, which the user must know about.
func TestCdCmdWarnsWhenPoliciesFileFailsToLoad(t *testing.T) {
	target := t.TempDir()
	origWD := restoreWD(t)
	eng := &policyEngine{err: errors.New(`load C:\cfg\policies.json: invalid character`)}
	out, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{target}, Cwd: origWD, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "已切换到") || !strings.Contains(out.Message, "权限规则文件") ||
		!strings.Contains(out.Message, "policies.json: invalid character") {
		t.Fatalf("no policies.json warning after /cd: %q", out.Message)
	}
}

func TestCdCmdNoPolicyWarningWhenLoaded(t *testing.T) {
	target := t.TempDir()
	origWD := restoreWD(t)
	out, _ := NewCdCmd().Execute(context.Background(), Input{Args: []string{target}, Cwd: origWD, Engine: &policyEngine{}})
	if strings.Contains(out.Message, "权限规则文件") {
		t.Fatalf("spurious warning: %q", out.Message)
	}
}
