package mcp

import "testing"

func TestValidateSTDIOCommandRejectsShellWrappers(t *testing.T) {
	for _, command := range []string{"sh", "bash", "cmd.exe", "powershell", "pwsh.exe"} {
		if err := validateSTDIOCommand(command); err == nil {
			t.Fatalf("validateSTDIOCommand(%q) expected error", command)
		}
	}
}

func TestValidateSTDIOCommandRejectsControlCharacters(t *testing.T) {
	for _, command := range []string{"node;rm", "node|sh", "node`whoami`", "node\nother"} {
		if err := validateSTDIOCommand(command); err == nil {
			t.Fatalf("validateSTDIOCommand(%q) expected error", command)
		}
	}
}

func TestValidateSTDIOCommandAllowsCommonLaunchers(t *testing.T) {
	for _, command := range []string{"node", "npx", "uvx", "docker", `C:\\Program Files\\nodejs\\node.exe`} {
		if err := validateSTDIOCommand(command); err != nil {
			t.Fatalf("validateSTDIOCommand(%q) unexpected error: %v", command, err)
		}
	}
}
