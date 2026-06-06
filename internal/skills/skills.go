package skills

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	FilePath     string   `json:"-"`
	Directory    string   `json:"-"`
}

type Manager struct {
	skills map[string]Skill
	dirs   []string
	mu     sync.RWMutex
}

type frontmatter struct {
	Name         string
	Description  string
	Paths        []string
	AllowedTools []string
	Body         string
}

func NewManager() *Manager { return &Manager{skills: make(map[string]Skill)} }

func (m *Manager) AddDirectory(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs = append(m.dirs, dir)
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
	content := string(data)
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	if name == "SKILL" || name == "skill" {
		name = filepath.Base(filepath.Dir(path))
	}
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
	if len(desc) > 120 {
		desc = desc[:117] + "..."
	}
	skill := Skill{Name: name, Description: desc, Prompt: body, FilePath: path, Directory: filepath.Dir(path)}
	if fm != nil {
		skill.Conditional = len(fm.Paths) > 0
		skill.Paths = fm.Paths
		skill.AllowedTools = fm.AllowedTools
	}
	m.skills[name] = skill
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
		}
	}
	return fm
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

func (m *Manager) Matching(ctx context.Context, s string) []Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var r []Skill
	for _, sk := range m.skills {
		if !sk.Conditional || strings.Contains(strings.ToLower(s), strings.ToLower(sk.Name)) {
			r = append(r, sk)
		}
	}
	return r
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

var bundles = []Skill{
	{Name: "batch", Description: "Parallel work orchestration across the codebase.", Prompt: "PARALLEL WORKFLOW:\n1. Break the task into independent sub-tasks\n2. For each sub-task, spawn an agent tool with clear instructions\n3. Each agent works independently and reports results\n4. Review all results and integrate them\n5. Verify the combined result is correct"},
	{Name: "debug", Description: "Systematic debugging: reproduce, isolate, fix, verify.", Prompt: "DEBUGGING WORKFLOW:\n1. Reproduce the issue with minimal steps\n2. Isolate the root cause using logs and test cases\n3. Apply the minimal fix\n4. Verify the fix with the original reproduction case\n5. Add a regression test if applicable", Conditional: true},
	{Name: "keybindings-help", Description: "Help users configure and understand keyboard shortcuts.", Prompt: "KEYBINDINGS HELP WORKFLOW:\n1. Identify the keybinding or terminal behavior the user is asking about\n2. Explain the relevant shortcut plainly\n3. If configuration is needed, inspect the local config before editing\n4. Prefer small, reversible config changes\n5. Verify the updated keybinding behavior when possible"},
	{Name: "lorem-ipsum", Description: "Generate realistic placeholder copy for UI, docs, and tests.", Prompt: "PLACEHOLDER CONTENT WORKFLOW:\n1. Infer the product/domain and audience\n2. Generate copy that matches the requested length and tone\n3. Avoid generic filler when realistic domain copy is more useful\n4. Keep placeholder data clearly non-production\n5. Preserve any requested structure or formatting"},
	{Name: "refactor", Description: "Safe refactoring in small, reversible steps.", Prompt: "REFACTORING WORKFLOW:\n1. Understand the existing code and its tests\n2. Plan the refactoring in small, reversible steps\n3. Run tests after each step\n4. Keep the public API stable\n5. Update documentation if behavior changes", Conditional: true},
	{Name: "review", Description: "Code review: correctness, style, performance, security.", Prompt: "CODE REVIEW CHECKLIST:\n1. Correctness: does the logic handle edge cases?\n2. Style: does it follow project conventions?\n3. Performance: any O(n²) issues or unnecessary allocations?\n4. Security: input validation, error handling, secret management\n5. Tests: are the important paths covered?", Conditional: true},
	{Name: "test", Description: "Write comprehensive tests: unit, integration, edge cases.", Prompt: "TESTING WORKFLOW:\n1. Start with the happy path\n2. Add edge cases (empty, nil, boundary values)\n3. Test error conditions\n4. Use table-driven tests for multiple cases\n5. Aim for meaningful coverage, not 100%", Conditional: true},
	{Name: "verify", Description: "Verify changes: build, lint, test, manual check.", Prompt: "VERIFICATION CHECKLIST:\n1. Build succeeds without errors or warnings\n2. Lint/typecheck passes\n3. Tests pass (existing + new)\n4. Manual verification if applicable\n5. No debug code or comments left behind\n6. Git status is clean or changes are intentional", Conditional: true},
	{Name: "simplify", Description: "Code review and cleanup: find duplicates and anti-patterns.", Prompt: "SIMPLIFICATION WORKFLOW:\n1. Search the codebase for duplicate logic or utilities\n2. Identify code that can be replaced with existing functions\n3. Check for anti-patterns and over-engineering\n4. Propose simplifications with before/after comparisons\n5. Apply changes one at a time, testing after each"},
	{Name: "remember", Description: "Memory review: deduplicate and clean up CLAUDE.md entries.", Prompt: "MEMORY REVIEW WORKFLOW:\n1. Read CLAUDE.md and CLAUDE.local.md files\n2. Identify duplicate or conflicting entries\n3. Flag outdated information\n4. Propose cleaned-up versions\n5. Do NOT apply changes automatically — present proposals to user first"},
	{Name: "update-config", Description: "Read and modify cove configuration files.", Prompt: "CONFIG UPDATE WORKFLOW:\n1. Read the existing config file (~/.cove/config.json)\n2. Identify the setting to change\n3. Make the change precisely\n4. Validate the JSON is still valid\n5. Inform the user of the change (restart may be required)"},
	{Name: "loop", Description: "Schedule recurring prompts at intervals (5m, 2h, 1d).", Prompt: "RECURRING TASK WORKFLOW:\n1. Parse the interval from user input (5m, 30m, 2h, 1d)\n2. Parse the prompt to execute\n3. Use the cron tool to schedule the task\n4. Confirm the schedule with the user"},
	{Name: "schedule", Description: "Schedule remote or background agent work when supported.", Prompt: "SCHEDULE WORKFLOW:\n1. Clarify the task, cadence, and expected output\n2. Prefer local cron/task tools when remote scheduling is unavailable\n3. Record the schedule with clear status and boundaries\n4. Tell the user whether it persists after process exit\n5. Provide the command or task id needed to inspect it later"},
	{Name: "skillify", Description: "Capture workflow patterns as reusable skills.", Prompt: "SKILL CREATION WORKFLOW:\n1. Review the conversation to identify a repeatable workflow pattern\n2. Extract the key steps and decisions\n3. Write a clear, instructional prompt\n4. Create the skill using the memory tool\n5. The skill file goes in ~/.cove/skills/<name>/SKILL.md"},
	{Name: "stuck", Description: "Diagnose frozen or stuck session.", Prompt: "STUCK SESSION WORKFLOW:\n1. Check if the current task is looping or stuck\n2. Try a different approach or break the task into smaller steps\n3. Use bash to check for hung processes\n4. If truly stuck, compact the conversation and restart the task\n5. Report what caused the issue for future reference"},
	{Name: "claude-api", Description: "Guide implementation against Claude/OpenAI-compatible API patterns.", Prompt: "API IMPLEMENTATION WORKFLOW:\n1. Identify the provider, endpoint shape, streaming mode, and auth requirements\n2. Prefer existing provider abstractions in the repo\n3. Validate request/response schemas with tests\n4. Handle retries, rate limits, and streaming parse errors explicitly\n5. Document required environment variables and configuration"},
	{Name: "claude-in-chrome", Description: "Browser-based verification workflow for web UI tasks.", Prompt: "BROWSER VERIFICATION WORKFLOW:\n1. Start or locate the local app URL\n2. Use browser automation to load the relevant screen\n3. Interact with the changed flow, not just the landing page\n4. Capture screenshots or concrete observations\n5. Report visual/layout issues and console/runtime errors"},
	{Name: "init", Description: "Initialize a new project with cove setup.", Prompt: "INITIALIZATION WORKFLOW:\n1. Create .claude directory with skills/ and commands/ subdirectories\n2. Create initial CLAUDE.md with project overview\n3. Create .cove.json with project-specific settings\n4. Report what was set up for the user"},
	{Name: "commit", Description: "Generate meaningful commit messages from code changes.", Prompt: "COMMIT WORKFLOW:\n1. Run git status and git diff to understand the changes\n2. Analyze the diff: what changed, why, impact\n3. Generate a conventional commit message (type: description)\n4. Format: feat/fix/refactor/test/docs/chore: short description\n5. Stage changes and create the commit"},
	{Name: "perf", Description: "Performance profiling and optimization workflow.", Prompt: "PERFORMANCE WORKFLOW:\n1. Identify the bottleneck (CPU, memory, I/O)\n2. Use profiling tools or benchmarks to get data\n3. Find the hot path and quantify the problem\n4. Apply the fix and measure the improvement\n5. Document the optimization and before/after numbers"},
}

func RegisterBundles(m *Manager) {
	for _, s := range bundles {
		m.Register(s)
	}
}

func LoadAll(m *Manager, cwd string) {
	RegisterBundles(m)

	home, _ := os.UserHomeDir()
	if home != "" {
		m.AddDirectory(filepath.Join(home, ".cove", "skills"))
		m.AddDirectory(filepath.Join(home, ".claude", "skills"))
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
