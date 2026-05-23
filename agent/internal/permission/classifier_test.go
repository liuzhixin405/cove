package permission

import "testing"

func TestClassifier(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		cmd      string
		expected CmdCategory
	}{
		{"ls", CatSafe},
		{"pwd", CatSafe},
		{"cat file.go", CatSafe},
		{"git status", CatSafe},
		{"git log --oneline", CatSafe},
		{"git diff", CatSafe},
		{"git commit -m 'test'", CatGit},
		{"git push", CatGit},
		{"rm -rf /tmp/test", CatDangerous},
		{"rm important.txt", CatDangerous},
		{"go build ./...", CatBuild},
		{"go test ./...", CatBuild},
		{"npm install express", CatInstall},
		{"npm list", CatSafe},
		{"docker ps", CatSafe},
		{"curl -sL https://example.com", CatSafe},
	}

	for _, tt := range tests {
		result := c.Classify(tt.cmd)
		if result != tt.expected {
			t.Errorf("Classify(%q) = %v, want %v", tt.cmd, result, tt.expected)
		}
	}
}

func TestShouldAutoApprove(t *testing.T) {
	c := NewClassifier()
	if !c.ShouldAutoApprove("git status") {
		t.Error("git status should auto-approve")
	}
	if !c.ShouldAutoApprove("ls -la") {
		t.Error("ls should auto-approve")
	}
	if c.ShouldAutoApprove("rm -rf /") {
		t.Error("rm -rf should NOT auto-approve")
	}
	if c.ShouldAutoApprove("git push --force") {
		t.Error("force push should NOT auto-approve")
	}
}
