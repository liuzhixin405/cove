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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/safepath"
	"github.com/liuzhixin405/cove/internal/safeurl"
	"github.com/liuzhixin405/cove/internal/textutil"
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
	Source    string   `json:"-"`
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

// AddDirectory loads every skill in dir as a user skill. A skill with the same
// name as one already loaded replaces it.
func (m *Manager) AddDirectory(dir string) { m.addDirectory(dir, SourceUser) }

func (m *Manager) addDirectory(dir, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			m.loadSkillFile(filepath.Join(dir, e.Name(), "SKILL.md"), source)
		}
		if strings.HasSuffix(e.Name(), ".md") && !e.IsDir() {
			m.loadSkillFile(filepath.Join(dir, e.Name()), source)
		}
	}
}

func (m *Manager) loadSkillFile(path, source string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	if name == "SKILL" || name == "skill" {
		name = filepath.Base(filepath.Dir(path))
	}
	sk := parseSkill(name, string(data), path)
	sk.Source = source
	m.skills[sk.Name] = sk
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
		desc = textutil.ClipRunes(desc, 120)
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
		k, v := strings.TrimSpace(parts[0]), unquote(strings.TrimSpace(parts[1]))
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

// unquote removes one pair of matching surrounding quotes. Values are split
// on commas afterwards, so a quoted list like "*.go,*.py" would otherwise leave
// a stray quote on its first and last patterns, which then match nothing.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
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
	fmt.Fprintf(&sb, "[Skill: %s]\n\n", s.Name)
	if len(s.Steps) > 0 {
		sb.WriteString("Follow these steps in order. Do not skip ahead or reorder them — treat each one as a checkpoint to complete (and verify) before starting the next:\n")
		for i, step := range s.Steps {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, step)
		}
		sb.WriteString("\n")
	}
	if len(s.AllowedTools) > 0 {
		fmt.Fprintf(&sb, "While executing this skill, only use these tools: %s. If the task genuinely requires a different tool, explain why before using it.\n\n", strings.Join(s.AllowedTools, ", "))
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

// All returns every loaded skill, sorted by name.
func (m *Manager) All() []Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sortedLocked()
}

func (m *Manager) sortedLocked() []Skill {
	r := make([]Skill, 0, len(m.skills))
	for _, s := range m.skills {
		r = append(r, s)
	}
	sort.Slice(r, func(i, j int) bool { return r[i].Name < r[j].Name })
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

func (m *Manager) BuildPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.skills) == 0 {
		return ""
	}
	// Sorted: this text is part of the system prompt, and map order differs
	// on every start, which made the prompt differ between sessions.
	var sb strings.Builder
	sb.WriteString("\n\n<available_skills>\n")
	sb.WriteString("Load a skill with the skill tool when it fits the task at hand.\n")
	for _, s := range m.sortedLocked() {
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

// installHTTPClient and the registry fetch both use the shared SSRF-hardened
// client: these are the only outbound requests the skills package makes, and
// they were previously plain http.Clients that would follow a redirect to an
// internal address without complaint.
var installHTTPClient = safeurl.NewClient(10 * time.Second)

func FetchRegistry() ([]RegistryEntry, error) {
	c := safeurl.NewClient(10 * time.Second)
	resp, err := c.Get(RegistryURL)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var entries []RegistryEntry
		if json.Unmarshal(data, &entries) == nil {
			return entries, nil
		}
	}
	var fallback []RegistryEntry
	_ = json.Unmarshal([]byte(fallbackJSON), &fallback)
	return fallback, nil
}

func InstallSkill(name, source, url string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("cannot locate home directory")
	}
	// The name becomes a directory under ~/.cove/skills. It comes from a slash
	// command argument or from the REMOTE skills registry, so joining it
	// unchecked let "../../../.ssh" create that directory and write a file into
	// it. See internal/safepath.
	dir, err := safepath.Join("skill", filepath.Join(home, ".cove", "skills"), name)
	if err != nil {
		return err
	}

	var content string
	if source == "url" && url != "" {
		// The registry is fetched over the network, so its URLs are untrusted.
		// Without this the fetch happily followed a registry entry pointing at
		// http://169.254.169.254/... and wrote the cloud metadata response into
		// a skill file — which is then injected into the model's system prompt.
		if err := safeurl.ValidateURL(url); err != nil {
			return fmt.Errorf("refusing to download skill %q: %w", name, err)
		}
		resp, err := installHTTPClient.Get(url)
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
		if err != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
		content = string(data)
	} else {
		content = fmt.Sprintf("# %s\n\nSkill: %s\n\nInstructions to be filled by the user.\n", name, name)
	}

	// Create the directory only once the content is in hand, so a failed
	// download does not leave an empty skill directory behind.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644)
}

// embeddedSkills holds the built-in skills compiled into the binary. They are
// loaded straight into memory (LoadEmbedded), never copied to disk: a copy in
// the user directory would override every later built-in version, so skills
// would stop updating with the binary.
//
//go:embed embedded
var embeddedSkills embed.FS

// LoadAll loads skills from every source, lowest precedence first; a later
// definition with the same name replaces an earlier one, so the closer
// definition wins:
//
//  1. built-in skills compiled into the binary
//  2. skills bundled with enabled plugins (~/.cove/plugins/<name>/skills)
//  3. the user's own (~/.claude/skills, then ~/.cove/skills)
//  4. the project's: every directory from the repository root down to cwd,
//     .claude/skills then .cove/skills in each, the innermost last
//
// Nothing above the repository root is read (without a repository, only cwd
// itself), and subdirectories below cwd are never scanned: a vendored or cloned
// dependency that ships a skills folder must not inject skills into the session.
func LoadAll(m *Manager, cwd string) {
	m.LoadEmbedded()

	home, _ := os.UserHomeDir()
	if home != "" {
		pluginsDir := filepath.Join(home, ".cove", "plugins")
		if entries, err := os.ReadDir(pluginsDir); err == nil {
			for _, e := range entries {
				// Directories suffixed .disabled honour the plugin enable/disable state.
				if !e.IsDir() || strings.HasSuffix(e.Name(), ".disabled") {
					continue
				}
				m.addDirectory(filepath.Join(pluginsDir, e.Name(), "skills"), SourcePlugin)
			}
		}
		m.addDirectory(filepath.Join(home, ".claude", "skills"), SourceUser)
		m.addDirectory(filepath.Join(home, ".cove", "skills"), SourceUser)
	}

	for _, dir := range projectDirs(cwd) {
		m.addDirectory(filepath.Join(dir, ".claude", "skills"), SourceProject)
		m.addDirectory(filepath.Join(dir, ".cove", "skills"), SourceProject)
	}
}

// projectDirs returns the directories from the repository root down to cwd,
// outermost first, or just cwd when it is not inside a repository.
func projectDirs(cwd string) []string {
	if cwd == "" {
		return nil
	}
	dirs := []string{cwd}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			// Reverse: repository root first, cwd last.
			for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
				dirs[i], dirs[j] = dirs[j], dirs[i]
			}
			return dirs
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return []string{cwd} // no repository: the project is cwd alone
		}
		dir = parent
		dirs = append(dirs, dir)
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
		sk.Source = SourceBuiltin
		m.skills[sk.Name] = sk
	}
}

// Skill sources, lowest precedence first.
const (
	SourceBuiltin = "builtin"
	SourcePlugin  = "plugin"
	SourceUser    = "user"
	SourceProject = "project"
)

// Disable removes the named skills (config "disabled_skills"). Unknown names
// are ignored.
func (m *Manager) Disable(names ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range names {
		delete(m.skills, strings.TrimSpace(n))
	}
}

// ExportBuiltin writes an editable copy of a built-in skill to
// ~/.cove/skills/<name>/SKILL.md and returns its path. The copy overrides the
// built-in from then on — including later versions shipped with the binary —
// until it is deleted. It never overwrites an existing file.
func ExportBuiltin(name string) (string, error) {
	data, err := embeddedSkills.ReadFile("embedded/" + name + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("no built-in skill named %q", name)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot locate home directory")
	}
	dir, err := safepath.Join("skill", filepath.Join(home, ".cove", "skills"), name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists; edit or delete it instead", path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
