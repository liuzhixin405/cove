package safety

import "testing"

// TestCatastrophicCommandsBlock is the regression test for the checker that
// never blocked: every dangerous command was reported at SevWarning, which the
// engine only logs, so `rm -rf /` passed through the "safety" gate.
func TestCatastrophicCommandsBlock(t *testing.T) {
	c := New()

	for _, cmd := range []string{
		"rm -rf /",
		"rm -rf ~",
		"rm -rf --no-preserve-root /",
		"dd if=/dev/zero of=/dev/sda",
		"mkfs.ext4 /dev/sda1",
		"chmod 777 /",
	} {
		res := c.Scan(cmd, "bash")
		if res.BlockingFinding() == nil {
			t.Errorf("%q produced no blocking finding", cmd)
		}
		if res.Passed {
			t.Errorf("%q was reported as passing the safety scan", cmd)
		}
	}
}

// TestRiskyCommandsWarnOnly confirms project-scoped destructive commands are
// surfaced but not hard-blocked — they are routinely legitimate.
func TestRiskyCommandsWarnOnly(t *testing.T) {
	c := New()

	for _, cmd := range []string{
		"git reset --hard HEAD~1",
		"git push --force origin feature",
		"rm -rf .",
		"rm -rf *",
	} {
		res := c.Scan(cmd, "bash")
		if res.BlockingFinding() != nil {
			t.Errorf("%q was hard-blocked, want warning only", cmd)
		}
		if len(res.Findings) == 0 {
			t.Errorf("%q produced no finding at all", cmd)
		}
		if res.WorstSeverity() != SevWarning {
			t.Errorf("%q worst severity = %v, want warning", cmd, res.WorstSeverity())
		}
	}
}

// TestBenignCommandsClean guards against the checker firing on ordinary work.
func TestBenignCommandsClean(t *testing.T) {
	c := New()
	for _, cmd := range []string{
		"go test ./...",
		"git status",
		"rm -rf ./node_modules/.cache",
		"rm -rf ./build",
		"rm -rf dist",
		"ls -la",
	} {
		res := c.Scan(cmd, "bash")
		for _, f := range res.Findings {
			if f.Rule == "dangerous_command" {
				t.Errorf("%q wrongly flagged: %s", cmd, f.Message)
			}
		}
	}
}
