package permission

import "testing"

// TestShouldAutoApprove_RedirectWithoutSpace is the H-10 regression: output
// redirection written without a leading space (echo x>file) writes to disk, yet
// the control-operator list only contained " >" (with a space), so these commands
// were classified as read-only and auto-approved.
func TestShouldAutoApprove_RedirectWithoutSpace(t *testing.T) {
	c := NewClassifier()
	blocked := []string{
		"echo x>/etc/passwd",
		"ls>out.txt",
		"cat a>b",
		"echo data>>~/.bashrc",
	}
	for _, cmd := range blocked {
		if c.ShouldAutoApprove(cmd) {
			t.Errorf("%q writes via redirect and must NOT be auto-approved", cmd)
		}
	}
}
