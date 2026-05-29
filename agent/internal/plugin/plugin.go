package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type State int

const (
	Disabled State = iota
	Enabled
	Error
)

type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author,omitempty"`
	Commands    []string `json:"commands,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Hooks       []string `json:"hooks,omitempty"`
	Skills      []string `json:"skills,omitempty"`
}

type Entry struct {
	Manifest Manifest
	Dir      string
	State    State
	Error    string
}

type Manager struct {
	plugins     map[string]*Entry
	dir         string
	marketplace *Marketplace
	mu          sync.RWMutex
}

func NewManager() *Manager {
	home, _ := os.UserHomeDir()
	return &Manager{
		plugins: make(map[string]*Entry),
		dir:     filepath.Join(home, ".agentgo", "plugins"),
	}
}

func (m *Manager) Init() {
	os.MkdirAll(m.dir, 0755)
	m.marketplace = NewMarketplace(m.dir)
	m.scanPlugins()
}

func (m *Manager) Refresh() {
	m.mu.Lock()
	m.plugins = make(map[string]*Entry)
	m.mu.Unlock()
	m.scanPlugins()
}

func (m *Manager) Dir() string {
	return m.dir
}

func (m *Manager) scanPlugins() {
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := m.loadPlugin(e.Name()); err != nil {
			m.recordPluginError(e.Name(), err)
		}
	}
}

func (m *Manager) loadPlugin(name string) error {
	state := Enabled
	dirName := name
	if strings.HasSuffix(name, ".disabled") {
		state = Disabled
		dirName = strings.TrimSuffix(name, ".disabled")
	}
	dir := filepath.Join(m.dir, name)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Name == "" {
		manifest.Name = dirName
	}
	m.mu.Lock()
	m.plugins[manifest.Name] = &Entry{
		Manifest: manifest,
		Dir:      dir,
		State:    state,
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) recordPluginError(name string, err error) {
	dirName := name
	state := Error
	if strings.HasSuffix(name, ".disabled") {
		dirName = strings.TrimSuffix(name, ".disabled")
	}
	m.mu.Lock()
	m.plugins[dirName] = &Entry{
		Manifest: Manifest{Name: dirName, Version: "unknown", Description: "Plugin could not be loaded"},
		Dir:      filepath.Join(m.dir, name),
		State:    state,
		Error:    err.Error(),
	}
	m.mu.Unlock()
}

func (m *Manager) Install(name string, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pluginDir := filepath.Join(m.dir, name)
	if _, err := os.Stat(pluginDir); err == nil {
		return fmt.Errorf("plugin %s already installed", name)
	}
	if _, err := os.Stat(pluginDir + ".disabled"); err == nil {
		return fmt.Errorf("plugin %s already installed", name)
	}

	// Remote install via git clone if URL looks like a git repo
	if url != "" && looksLikeGitRepo(url) {
		args := []string{"clone", "--depth=1", "--quiet", url, pluginDir}
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(pluginDir)
			return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
		}
		// Validate manifest
		if _, err := os.Stat(filepath.Join(pluginDir, "manifest.json")); os.IsNotExist(err) {
			os.RemoveAll(pluginDir)
			return fmt.Errorf("cloned repo has no manifest.json — not a valid plugin")
		}
		return m.loadPluginLocked(name)
	}

	// Local scaffold (legacy fallback)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		return err
	}

	defaultManifest := Manifest{
		Name:        name,
		Version:     "0.1.0",
		Description: fmt.Sprintf("Plugin: %s", name),
	}
	data, err := json.MarshalIndent(defaultManifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "manifest.json"), data, 0644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	m.plugins[name] = &Entry{
		Manifest: defaultManifest,
		Dir:      pluginDir,
		State:    Enabled,
	}
	return nil
}

func (m *Manager) loadPluginLocked(name string) error {
	dir := filepath.Join(m.dir, name)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Name == "" {
		manifest.Name = name
	}
	m.plugins[manifest.Name] = &Entry{
		Manifest: manifest,
		Dir:      dir,
		State:    Enabled,
	}
	return nil
}

func (m *Manager) Uninstall(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.plugins[name]
	if !ok {
		return fmt.Errorf("plugin %s not found", name)
	}
	delete(m.plugins, name)

	if err := os.RemoveAll(p.Dir); err != nil {
		return err
	}
	if err := os.RemoveAll(strings.TrimSuffix(p.Dir, ".disabled") + ".disabled"); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[name]
	if !ok {
		return fmt.Errorf("plugin %s not found", name)
	}
	if p.State == Disabled {
		return nil
	}
	disabledDir := p.Dir + ".disabled"
	_ = os.RemoveAll(disabledDir)
	if err := os.Rename(p.Dir, disabledDir); err != nil {
		return err
	}
	p.Dir = disabledDir
	p.State = Disabled
	return nil
}

func (m *Manager) Enable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[name]
	if !ok {
		return fmt.Errorf("plugin %s not found", name)
	}
	if p.State == Enabled {
		return nil
	}
	enabledDir := strings.TrimSuffix(p.Dir, ".disabled")
	_ = os.RemoveAll(enabledDir)
	if err := os.Rename(p.Dir, enabledDir); err != nil {
		return err
	}
	p.Dir = enabledDir
	p.State = Enabled
	return nil
}

func (m *Manager) AllPlugins() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []Entry
	for _, p := range m.plugins {
		result = append(result, *p)
	}
	return result
}

func (m *Manager) EnabledTools() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []string
	for _, p := range m.plugins {
		if p.State != Enabled {
			continue
		}
		tools = append(tools, p.Manifest.Tools...)
	}
	return tools
}

func (m *Manager) EnabledCommands() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var cmds []string
	for _, p := range m.plugins {
		if p.State != Enabled {
			continue
		}
		cmds = append(cmds, p.Manifest.Commands...)
	}
	return cmds
}

// --- Marketplace bridge methods (satisfy command interface assertions) ---

// MarketplaceSearch searches the plugin marketplace.
func (m *Manager) MarketplaceSearch(query string) string {
	if m.marketplace == nil {
		return "marketplace 未初始化"
	}
	entries := m.marketplace.Search(query)
	if len(entries) == 0 {
		if query != "" {
			return fmt.Sprintf("未找到匹配 %q 的插件 (试试 /plugin refresh 更新索引)", query)
		}
		return "marketplace 索引为空 (试试 /plugin refresh)"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("找到 %d 个插件:\n", len(entries)))
	for i, e := range entries {
		if i >= 20 {
			sb.WriteString(fmt.Sprintf("  ... 还有 %d 个\n", len(entries)-20))
			break
		}
		installed := ""
		if _, err := os.Stat(filepath.Join(m.dir, e.Name)); err == nil {
			installed = " [已安装]"
		}
		sb.WriteString(fmt.Sprintf("  %-20s %s v%s%s\n", e.Name, e.Description, e.Version, installed))
		if e.Author != "" {
			sb.WriteString(fmt.Sprintf("  %24s by %s\n", "", e.Author))
		}
	}
	sb.WriteString("\n安装: /plugin install <名称>")
	return sb.String()
}

// MarketplaceRefresh updates the marketplace index.
func (m *Manager) MarketplaceRefresh() error {
	if m.marketplace == nil {
		return fmt.Errorf("marketplace 未初始化")
	}
	return m.marketplace.Refresh()
}

// MarketplaceUpdate updates one or all plugins.
func (m *Manager) MarketplaceUpdate(name string) (string, error) {
	if m.marketplace == nil {
		return "", fmt.Errorf("marketplace 未初始化")
	}
	if name != "" {
		if err := m.marketplace.Update(name); err != nil {
			return "", err
		}
		lock, _ := m.marketplace.LockInfo(name)
		return fmt.Sprintf("✓ %s 已更新到 %s (%s)", name, lock.Version, lock.CommitSHA[:7]), nil
	}
	// Update all
	updated, errs := m.marketplace.UpdateAll()
	var sb strings.Builder
	if len(updated) > 0 {
		sb.WriteString(fmt.Sprintf("✓ 已更新 %d 个插件: %s\n", len(updated), strings.Join(updated, ", ")))
	} else {
		sb.WriteString("所有插件已是最新\n")
	}
	if len(errs) > 0 {
		sb.WriteString(fmt.Sprintf("⚠ %d 个失败: %s\n", len(errs), strings.Join(errs, "; ")))
	}
	return sb.String(), nil
}

// Marketplace returns the marketplace instance (for advanced usage).
func (m *Manager) Marketplace() *Marketplace {
	return m.marketplace
}

// MarketplaceInstall installs a plugin from the marketplace by name.
func (m *Manager) MarketplaceInstall(name string) error {
	if m.marketplace == nil {
		return fmt.Errorf("marketplace 未初始化")
	}
	if err := m.marketplace.InstallFromMarketplace(name); err != nil {
		return err
	}
	// Reload into plugin map
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadPluginLocked(name)
}

// looksLikeGitRepo returns true if the URL appears to be a git repository.
// More strict than isGitURL — requires .git suffix, git@ prefix, or known git hosts.
func looksLikeGitRepo(url string) bool {
	if strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://") {
		return true
	}
	if strings.HasSuffix(url, ".git") {
		return true
	}
	// Known git hosting platforms
	knownHosts := []string{"github.com", "gitlab.com", "bitbucket.org", "gitee.com", "codeberg.org"}
	for _, host := range knownHosts {
		if strings.Contains(url, host+"/") {
			return true
		}
	}
	return false
}
