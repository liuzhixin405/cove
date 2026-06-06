package onboarding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// State tracks the onboarding status of a project.
type State struct {
	ProjectDir     string
	HasClaudeMD    bool
	HasGit         bool
	HasPackageJSON bool
	HasGoMod       bool
	Language       string
}

// Check performs onboarding detection for the given project directory.
func Check(projectDir string) *State {
	s := &State{ProjectDir: projectDir}
	s.HasClaudeMD = fileExists(filepath.Join(projectDir, "CLAUDE.md")) ||
		fileExists(filepath.Join(projectDir, ".claude", "CLAUDE.md"))
	s.HasGit = dirExists(filepath.Join(projectDir, ".git"))
	s.HasPackageJSON = fileExists(filepath.Join(projectDir, "package.json"))
	s.HasGoMod = fileExists(filepath.Join(projectDir, "go.mod"))
	s.Language = detectLanguage(projectDir)
	return s
}

// NeedsOnboarding returns true if the project doesn't have a CLAUDE.md yet.
func (s *State) NeedsOnboarding() bool {
	return !s.HasClaudeMD
}

// GenerateClaudeMD creates a starter CLAUDE.md based on detected project structure.
func (s *State) GenerateClaudeMD() string {
	var sb strings.Builder
	sb.WriteString("# Project Guide\n\n")
	sb.WriteString("## Overview\n\n")
	sb.WriteString(fmt.Sprintf("Language: %s\n", s.Language))

	if s.HasGit {
		sb.WriteString("Version Control: Git\n")
	}

	sb.WriteString("\n## Build & Run\n\n")
	switch {
	case s.HasGoMod:
		sb.WriteString("```bash\ngo build ./...\ngo test ./...\n```\n")
	case s.HasPackageJSON:
		sb.WriteString("```bash\nnpm install\nnpm run build\nnpm test\n```\n")
	default:
		sb.WriteString("<!-- Add build commands here -->\n")
	}

	sb.WriteString("\n## Conventions\n\n")
	sb.WriteString("<!-- Add project conventions, coding style, and important notes here -->\n")

	return sb.String()
}

// InitProject creates the CLAUDE.md file if it doesn't exist.
// Returns the path of the created file, or empty string if already exists.
func (s *State) InitProject() (string, error) {
	if s.HasClaudeMD {
		return "", nil
	}

	content := s.GenerateClaudeMD()
	path := filepath.Join(s.ProjectDir, "CLAUDE.md")

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to create CLAUDE.md: %w", err)
	}
	return path, nil
}

// Summary returns a brief description of what was detected.
func (s *State) Summary() string {
	var parts []string
	if s.HasGit {
		parts = append(parts, "git")
	}
	if s.Language != "" {
		parts = append(parts, s.Language)
	}
	if s.HasClaudeMD {
		parts = append(parts, "CLAUDE.md ✓")
	} else {
		parts = append(parts, "no CLAUDE.md")
	}
	return strings.Join(parts, " | ")
}

func detectLanguage(dir string) string {
	checks := []struct {
		file string
		lang string
	}{
		{"go.mod", "Go"},
		{"Cargo.toml", "Rust"},
		{"package.json", "JavaScript/TypeScript"},
		{"requirements.txt", "Python"},
		{"pyproject.toml", "Python"},
		{"pom.xml", "Java"},
		{"build.gradle", "Java/Kotlin"},
		{"Gemfile", "Ruby"},
		{"*.csproj", "C#"},
		{"mix.exs", "Elixir"},
	}
	for _, c := range checks {
		if strings.Contains(c.file, "*") {
			matches, _ := filepath.Glob(filepath.Join(dir, c.file))
			if len(matches) > 0 {
				return c.lang
			}
		} else if fileExists(filepath.Join(dir, c.file)) {
			return c.lang
		}
	}
	return "unknown"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
