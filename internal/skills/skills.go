package skills

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Skill struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Prompt       string   `json:"prompt"`
	Conditional  bool     `json:"conditional,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// Steps, if declared in frontmatter, is an explicit ordered checklist
	// for this skill (e.g. "Read the target file, Apply the fix, Run
	// tests, Verify output"). When present, SkillTool renders it as a
	// numbered checklist ahead of the skill's free-form prompt body, so a
	// skill can act as a lightweight structured workflow — a predetermined
	// sequence the model follows and checks off — rather than relying on
	// the model to re-derive the same steps from prose every time. This is
	// optional and purely additive: skills without "steps:" behave exactly
	// as before.
	Steps     []string `json:"steps,omitempty"`
	Builtin   bool     `json:"-"`
	FilePath  string   `json:"-"`
	Directory string   `json:"-"`
}

type Manager struct {
	skills map[string]Skill
	mu     sync.RWMutex
}

type frontmatter struct {
	Name         string
	Description  string
	Paths        []string
	AllowedTools []string
	Steps        []string
	Body         string
}

func NewManager() *Manager { return &Manager{skills: make(map[string]Skill)} }

func (m *Manager) AddDirectory(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scanDir(dir)
}

func (m *Manager) scanDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			m.loadSkillFromDir(filepath.Join(dir, e.Name()))
		}
		if strings.HasSuffix(e.Name(), ".md") && !e.IsDir() {
			m.loadSkillFile(filepath.Join(dir, e.Name()))
		}
	}
}

func (m *Manager) loadSkillFromDir(dir string) { m.loadSkillFile(filepath.Join(dir, "SKILL.md")) }

func (m *Manager) loadSkillFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	if name == "SKILL" || name == "skill" {
		name = filepath.Base(filepath.Dir(path))
	}
	m.skills[name] = parseSkill(name, string(data), path)
}

// parseSkill builds a Skill from raw SKILL.md content. It is a pure function
// (no locking, no I/O) so it can be reused for both on-disk and embedded skills.
func parseSkill(name, content, path string) Skill {
	fm := parseFrontmatter(content)
	body := content
	if fm != nil {
		body = fm.Body
		if fm.Name != "" {
			name = fm.Name
		}
	}
	desc := body
	if idx := strings.Index(body, "\n"); idx > 0 {
		desc = strings.TrimSpace(body[:idx])
	}
	if fm != nil && fm.Description != "" {
		desc = fm.Description
	}
	if len(desc) > 120 {
		desc = desc[:117] + "..."
	}
	skill := Skill{Name: name, Description: desc, Prompt: body, FilePath: path, Directory: filepath.Dir(path)}
	if fm != nil {
		skill.Conditional = len(fm.Paths) > 0
		skill.Paths = fm.Paths
		skill.AllowedTools = fm.AllowedTools
		skill.Steps = fm.Steps
	}
	return skill
}

func parseFrontmatter(content string) *frontmatter {
	if !strings.HasPrefix(content, "---\n") {
		return nil
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return nil
	}
	fmRaw := content[4 : 4+end]
	body := content[4+end+5:]
	fm := &frontmatter{Body: body}
	sc := bufio.NewScanner(strings.NewReader(fmRaw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		k, v := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		switch k {
		case "name":
			fm.Name = v
		case "description":
			fm.Description = v
		case "paths":
			for _, p := range strings.Split(v, ",") {
				fm.Paths = append(fm.Paths, strings.TrimSpace(p))
			}
		case "allowed_tools":
			for _, t := range strings.Split(v, ",") {
				fm.AllowedTools = append(fm.AllowedTools, strings.TrimSpace(t))
			}
		case "steps":
			for _, step := range strings.Split(v, ",") {
				if s := strings.TrimSpace(step); s != "" {
					fm.Steps = append(fm.Steps, s)
				}
			}
		}
	}
	return fm
}

// RenderInvocation builds the text handed back to the model when this
// skill is invoked via the "skill" tool: an explicit numbered checklist
// (if Steps is declared), an explicit tool-allowlist directive (if
// AllowedTools is declared), followed by the skill's free-form prompt
// body. Skills without Steps/AllowedTools render exactly as before
// (just the prompt body plus the trailing instruction line), so this is
// purely additive for skills that opt in.
func (s Skill) RenderInvocation() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Skill: %s]\n\n", s.Name))
	if len(s.Steps) > 0 {
		sb.WriteString("Follow these steps in order. Do not skip ahead or reorder them — treat each one as a checkpoint to complete (and verify) before starting the next:\n")
		for i, step := range s.Steps {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, step))
		}
		sb.WriteString("\n")
	}
	if len(s.AllowedTools) > 0 {
		sb.WriteString(fmt.Sprintf("While executing this skill, only use these tools: %s. If the task genuinely requires a different tool, explain why before using it.\n\n", strings.Join(s.AllowedTools, ", ")))
	}
	sb.WriteString(s.Prompt)
	sb.WriteString("\n\nFollow these instructions to complete the task.")
	return sb.String()
}

func (m *Manager) Register(skill Skill) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skills[skill.Name] = skill
}

func (m *Manager) Get(name string) (Skill, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.skills[name]
	return s, ok
}

func (m *Manager) All() []Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var r []Skill
	for _, s := range m.skills {
		r = append(r, s)
	}
	return r
}

// Matching returns skills whose Paths glob-patterns match the given file path.
// Only conditional skills (those with Paths defined) are considered.
func (m *Manager) Matching(ctx context.Context, filePath string) []Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var r []Skill
	for _, sk := range m.skills {
		if !sk.Conditional || len(sk.Paths) == 0 {
			continue
		}
		base := filepath.Base(filePath)
		for _, pattern := range sk.Paths {
			if matched, _ := filepath.Match(strings.TrimSpace(pattern), base); matched {
				r = append(r, sk)
				break
			}
		}
	}
	return r
}

// MatchingPrompt returns concatenated prompts of all skills matching a file path.
func (m *Manager) MatchingPrompt(filePath string) string {
	skills := m.Matching(context.Background(), filePath)
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n<relevant_skills>\n")
	for _, s := range skills {
		sb.WriteString("<skill name=\"" + s.Name + "\">\n")
		sb.WriteString(s.Prompt)
		sb.WriteString("\n</skill>\n")
	}
	sb.WriteString("</relevant_skills>\n")
	return sb.String()
}

func (m *Manager) BuildPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n<available_skills>\n")
	for _, s := range m.skills {
		sb.WriteString("<skill>\n  <name>" + s.Name + "</name>\n  <description>" + s.Description + "</description>\n")
		if len(s.AllowedTools) > 0 {
			sb.WriteString("  <allowed_tools>" + strings.Join(s.AllowedTools, ",") + "</allowed_tools>\n")
		}
		if len(s.Paths) > 0 {
			sb.WriteString("  <paths>" + strings.Join(s.Paths, ",") + "</paths>\n")
		}
		sb.WriteString("</skill>\n")
	}
	sb.WriteString("</available_skills>\n")
	return sb.String()
}

func (m *Manager) Count() int { m.mu.RLock(); defer m.mu.RUnlock(); return len(m.skills) }

type RegistryEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Author      string `json:"author,omitempty"`
}

var RegistryURL = "https://raw.githubusercontent.com/liuzhixin405/cove/main/skills-registry.json"
var fallbackJSON = `[{"name":"security-audit","description":"Security audit: scan deps, check vulnerabilities.","author":"marketplace"},{"name":"api-design","description":"REST API design: endpoints, schemas, OpenAPI.","author":"marketplace"},{"name":"dockerize","description":"Docker: Dockerfile, compose, build, push.","author":"marketplace"},{"name":"i18n","description":"Internationalization: extract strings, translations.","author":"marketplace"},{"name":"ci-cd","description":"CI/CD: Actions, pipelines, testing.","author":"marketplace"}]`
var installHTTPClient = &http.Client{Timeout: 10 * time.Second}

func FetchRegistry() ([]RegistryEntry, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(RegistryURL)
	if err == nil {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var entries []RegistryEntry
		if json.Unmarshal(data, &entries) == nil {
			return entries, nil
		}
	}
	var fallback []RegistryEntry
	json.Unmarshal([]byte(fallbackJSON), &fallback)
	return fallback, nil
}

func InstallSkill(name, source, url string) error {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cove", "skills", name)
	os.MkdirAll(dir, 0755)

	var content string
	if source == "url" && url != "" {
		resp, err := installHTTPClient.Get(url)
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
		content = string(data)
	} else {
		content = fmt.Sprintf("# %s\n\nSkill: %s\n\nInstructions to be filled by the user.\n", name, name)
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644)
}

// SeedDefaultSkills ensures built-in skills are present on disk.
// On first run, copies the bundled SKILL.md files to ~/.cove/skills/.
//
//go:embed embedded
var embeddedSkills embed.FS

func SeedDefaultSkills() {
	home, _ := os.UserHomeDir()
	if home == "" {
		return
	}
	dir := filepath.Join(home, ".cove", "skills")
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return // Already seeded
	}
	os.MkdirAll(dir, 0755)

	// Copy embedded skill files to ~/.cove/skills/
	entries, err := fs.ReadDir(embeddedSkills, "embedded")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		srcPath := "embedded/" + skillName + "/SKILL.md"
		data, err := embeddedSkills.ReadFile(srcPath)
		if err != nil {
			continue
		}
		dstDir := filepath.Join(dir, skillName)
		os.MkdirAll(dstDir, 0755)
		os.WriteFile(filepath.Join(dstDir, "SKILL.md"), data, 0644)
	}
}

func LoadAll(m *Manager, cwd string) {
	// Built-in skills are loaded directly from the embedded filesystem into
	// memory. This makes them available out of the box (no config required) and
	// keeps them in sync with the binary on every upgrade.
	m.LoadEmbedded()

	// User- and project-level skills are loaded next. Because AddDirectory
	// registers by name, a local skill with the same name transparently
	// overrides the built-in one, allowing customization.
	home, _ := os.UserHomeDir()
	if home != "" {
		m.AddDirectory(filepath.Join(home, ".cove", "skills"))
		m.AddDirectory(filepath.Join(home, ".claude", "skills"))
		// Installed plugins may bundle skills under plugins/<name>/skills/.
		// Scan each enabled plugin's skills directory so plugin skills become
		// available without manual symlinking. Directories suffixed .disabled
		// are skipped to honour the plugin enable/disable state.
		pluginsDir := filepath.Join(home, ".cove", "plugins")
		if entries, err := os.ReadDir(pluginsDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() || strings.HasSuffix(e.Name(), ".disabled") {
					continue
				}
				m.AddDirectory(filepath.Join(pluginsDir, e.Name(), "skills"))
			}
		}
	}

	if cwd != "" {
		m.AddDirectory(filepath.Join(cwd, ".claude", "skills"))
		m.AddDirectory(filepath.Join(cwd, ".cove", "skills"))

		entries, _ := os.ReadDir(cwd)
		for _, e := range entries {
			if e.IsDir() {
				m.AddDirectory(filepath.Join(cwd, e.Name(), ".claude", "skills"))
			}
		}

		dir := cwd
		for {
			parent := filepath.Dir(dir)
			m.AddDirectory(filepath.Join(parent, ".claude", "skills"))
			if parent == dir {
				break
			}
			dir = parent
		}
	}
}

// LoadEmbedded registers all built-in skills bundled with the binary directly
// into memory, without copying them to disk.
func (m *Manager) LoadEmbedded() {
	entries, err := fs.ReadDir(embeddedSkills, "embedded")
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		srcPath := "embedded/" + name + "/SKILL.md"
		data, err := embeddedSkills.ReadFile(srcPath)
		if err != nil {
			continue
		}
		sk := parseSkill(name, string(data), srcPath)
		sk.Builtin = true
		m.skills[sk.Name] = sk
	}
}
