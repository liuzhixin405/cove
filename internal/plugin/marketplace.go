package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MarketplaceEntry describes a plugin available in a marketplace.
type MarketplaceEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Version     string   `json:"version"`
	Source      string   `json:"source"` // git URL or local path
	Keywords    []string `json:"keywords,omitempty"`
	Category    string   `json:"category,omitempty"`
	Stars       int      `json:"stars,omitempty"`
	Downloads   int      `json:"downloads,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
}

// MarketplaceSource is a registry that provides plugin listings.
type MarketplaceSource struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "git", "url", "file", "directory"
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// Lockfile tracks installed plugin versions and sources.
type Lockfile struct {
	Plugins map[string]LockEntry `json:"plugins"`
}

// LockEntry records install metadata for a plugin.
type LockEntry struct {
	Source      string `json:"source"`
	Version     string `json:"version"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	InstalledAt string `json:"installed_at"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	AutoUpdate  bool   `json:"auto_update"`
}

// Marketplace manages plugin discovery and remote installation.
type Marketplace struct {
	mu       sync.Mutex
	dir      string // ~/.cove/plugins
	cacheDir string // ~/.cove/marketplace/cache
	sources  []MarketplaceSource
	index    []MarketplaceEntry // cached index from all sources
	lockfile Lockfile
}

const defaultMarketplaceRepo = "https://github.com/anthropics/claude-plugins-official.git"

// NewMarketplace creates a marketplace manager.
func NewMarketplace(pluginDir string) *Marketplace {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cove", "marketplace", "cache")
	os.MkdirAll(cacheDir, 0755)

	m := &Marketplace{
		dir:      pluginDir,
		cacheDir: cacheDir,
		lockfile: Lockfile{Plugins: make(map[string]LockEntry)},
	}
	m.loadSources()
	m.loadLockfile()
	m.loadCachedIndex()
	return m
}

// --- Sources Management ---

func (m *Marketplace) sourcesFile() string {
	return filepath.Join(filepath.Dir(m.dir), "marketplace", "sources.json")
}

func (m *Marketplace) loadSources() {
	data, err := os.ReadFile(m.sourcesFile())
	if err != nil {
		// Default: official marketplace
		m.sources = []MarketplaceSource{
			{Name: "official", Type: "git", URL: defaultMarketplaceRepo, Enabled: true},
		}
		return
	}
	json.Unmarshal(data, &m.sources)
	if len(m.sources) == 0 {
		m.sources = []MarketplaceSource{
			{Name: "official", Type: "git", URL: defaultMarketplaceRepo, Enabled: true},
		}
	}
}

func (m *Marketplace) saveSources() {
	os.MkdirAll(filepath.Dir(m.sourcesFile()), 0755)
	data, _ := json.MarshalIndent(m.sources, "", "  ")
	os.WriteFile(m.sourcesFile(), data, 0644)
}

// AddSource registers a new marketplace source.
func (m *Marketplace) AddSource(name, sourceType, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, s := range m.sources {
		if s.Name == name {
			return fmt.Errorf("source %q already exists", name)
		}
	}
	if sourceType != "git" && sourceType != "url" && sourceType != "file" && sourceType != "directory" {
		return fmt.Errorf("unsupported source type %q (use: git, url, file, directory)", sourceType)
	}

	m.sources = append(m.sources, MarketplaceSource{
		Name: name, Type: sourceType, URL: url, Enabled: true,
	})
	m.saveSources()
	return nil
}

// RemoveSource removes a marketplace source.
func (m *Marketplace) RemoveSource(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if name == "official" {
		return fmt.Errorf("cannot remove official marketplace")
	}
	for i, s := range m.sources {
		if s.Name == name {
			m.sources = append(m.sources[:i], m.sources[i+1:]...)
			m.saveSources()
			return nil
		}
	}
	return fmt.Errorf("source %q not found", name)
}

// Sources returns all configured marketplace sources.
func (m *Marketplace) Sources() []MarketplaceSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]MarketplaceSource, len(m.sources))
	copy(result, m.sources)
	return result
}

// --- Index / Search ---

func (m *Marketplace) indexFile() string {
	return filepath.Join(filepath.Dir(m.dir), "marketplace", "index.json")
}

func (m *Marketplace) loadCachedIndex() {
	data, err := os.ReadFile(m.indexFile())
	if err != nil {
		return
	}
	json.Unmarshal(data, &m.index)
}

func (m *Marketplace) saveIndex() {
	os.MkdirAll(filepath.Dir(m.indexFile()), 0755)
	data, _ := json.MarshalIndent(m.index, "", "  ")
	os.WriteFile(m.indexFile(), data, 0644)
}

// Refresh fetches the latest index from all enabled sources.
func (m *Marketplace) Refresh() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var allEntries []MarketplaceEntry
	var errs []string

	for _, src := range m.sources {
		if !src.Enabled {
			continue
		}
		entries, err := m.fetchSource(src)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", src.Name, err))
			continue
		}
		allEntries = append(allEntries, entries...)
	}

	m.index = allEntries
	m.saveIndex()

	if len(errs) > 0 {
		return fmt.Errorf("some sources failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *Marketplace) fetchSource(src MarketplaceSource) ([]MarketplaceEntry, error) {
	switch src.Type {
	case "git":
		return m.fetchGitSource(src)
	case "file":
		return m.fetchFileSource(src.URL)
	case "directory":
		return m.fetchDirectorySource(src.URL)
	default:
		return nil, fmt.Errorf("unsupported source type: %s", src.Type)
	}
}

// claudePluginJSON represents the .claude-plugin/plugin.json format used by
// anthropics/claude-plugins-official.
type claudePluginJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
	Homepage string `json:"homepage"`
}

func (m *Marketplace) fetchGitSource(src MarketplaceSource) ([]MarketplaceEntry, error) {
	repoDir := filepath.Join(m.cacheDir, sanitizeName(src.Name))

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		// Pull latest
		cmd := exec.Command("git", "-C", repoDir, "pull", "--ff-only")
		cmd.Stderr = os.Stderr
		cmd.Run() // best-effort
	} else {
		// Clone
		os.MkdirAll(filepath.Dir(repoDir), 0755)
		fmt.Fprintf(os.Stderr, "正在克隆 marketplace 源: %s ...\n", src.Name)
		cmd := exec.Command("git", "clone", "--depth=1", "--progress", src.URL, repoDir)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git clone %s failed: %w", src.URL, err)
		}
	}

	// Try registry.json first (flat index format)
	registryPath := filepath.Join(repoDir, "registry.json")
	if _, err := os.Stat(registryPath); err == nil {
		return m.fetchFileSource(registryPath)
	}

	// Fall back to Claude plugins official format:
	// scan plugins/*/.claude-plugin/plugin.json and external_plugins/*/.claude-plugin/plugin.json
	return m.fetchClaudePluginsRepo(repoDir, src.URL)
}

// fetchClaudePluginsRepo scans a repo with anthropics/claude-plugins-official layout.
func (m *Marketplace) fetchClaudePluginsRepo(repoDir, sourceURL string) ([]MarketplaceEntry, error) {
	var entries []MarketplaceEntry

	dirs := []string{
		filepath.Join(repoDir, "plugins"),
		filepath.Join(repoDir, "external_plugins"),
	}

	for _, dir := range dirs {
		subdirs, err := os.ReadDir(dir)
		if err != nil {
			continue // directory may not exist
		}
		for _, sub := range subdirs {
			if !sub.IsDir() {
				continue
			}
			pluginJSONPath := filepath.Join(dir, sub.Name(), ".claude-plugin", "plugin.json")
			data, err := os.ReadFile(pluginJSONPath)
			if err != nil {
				continue
			}
			var cp claudePluginJSON
			if err := json.Unmarshal(data, &cp); err != nil {
				continue
			}
			name := cp.Name
			if name == "" {
				name = sub.Name()
			}
			version := cp.Version
			if version == "" {
				version = "latest"
			}
			// Derive source URL for individual plugin install
			plugSrc := sourceURL
			if cp.Homepage != "" {
				plugSrc = cp.Homepage
			}
			entries = append(entries, MarketplaceEntry{
				Name:        name,
				Description: cp.Description,
				Author:      cp.Author.Name,
				Version:     version,
				Source:      plugSrc,
				Category:    filepath.Base(dir), // "plugins" or "external_plugins"
			})
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no registry.json and no .claude-plugin/plugin.json found in repo")
	}
	return entries, nil
}

func (m *Marketplace) fetchFileSource(path string) ([]MarketplaceEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []MarketplaceEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	return entries, nil
}

func (m *Marketplace) fetchDirectorySource(dir string) ([]MarketplaceEntry, error) {
	// Each subdirectory is a plugin with a manifest.json
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []MarketplaceEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var manifest Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}
		result = append(result, MarketplaceEntry{
			Name:        manifest.Name,
			Description: manifest.Description,
			Author:      manifest.Author,
			Version:     manifest.Version,
			Source:      filepath.Join(dir, e.Name()),
		})
	}
	return result, nil
}

// Search finds plugins matching a query in the cached index.
func (m *Marketplace) Search(query string) []MarketplaceEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	if query == "" {
		result := make([]MarketplaceEntry, len(m.index))
		copy(result, m.index)
		return result
	}

	q := strings.ToLower(query)
	var matches []MarketplaceEntry
	for _, entry := range m.index {
		score := 0
		if strings.Contains(strings.ToLower(entry.Name), q) {
			score += 10
		}
		if strings.Contains(strings.ToLower(entry.Description), q) {
			score += 5
		}
		if strings.Contains(strings.ToLower(entry.Category), q) {
			score += 3
		}
		for _, kw := range entry.Keywords {
			if strings.Contains(strings.ToLower(kw), q) {
				score += 2
			}
		}
		if score > 0 {
			matches = append(matches, entry)
		}
	}

	// Sort by relevance (name match first, then description)
	sort.Slice(matches, func(i, j int) bool {
		iName := strings.Contains(strings.ToLower(matches[i].Name), q)
		jName := strings.Contains(strings.ToLower(matches[j].Name), q)
		if iName != jName {
			return iName
		}
		return matches[i].Downloads > matches[j].Downloads
	})

	return matches
}

// List returns all available plugins from the cached index.
func (m *Marketplace) List() []MarketplaceEntry {
	return m.Search("")
}

// --- Remote Installation ---

// InstallFromMarketplace installs a plugin by name from the registry.
func (m *Marketplace) InstallFromMarketplace(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find in index
	var entry *MarketplaceEntry
	for i := range m.index {
		if strings.EqualFold(m.index[i].Name, name) {
			entry = &m.index[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("plugin %q not found in marketplace (try: /plugin refresh)", name)
	}

	return m.installFromSource(entry.Name, entry.Source, entry.Version)
}

// InstallFromGit installs a plugin directly from a git URL.
func (m *Marketplace) InstallFromGit(url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Derive name from URL (last path segment without .git)
	name := deriveNameFromURL(url)
	if name == "" {
		return fmt.Errorf("cannot derive plugin name from URL: %s", url)
	}

	return m.installFromSource(name, url, "")
}

func (m *Marketplace) installFromSource(name, source, version string) error {
	pluginDir := filepath.Join(m.dir, name)

	// Check already installed
	if _, err := os.Stat(pluginDir); err == nil {
		return fmt.Errorf("plugin %q already installed (use /plugin update %s)", name, name)
	}
	if _, err := os.Stat(pluginDir + ".disabled"); err == nil {
		return fmt.Errorf("plugin %q already installed (disabled)", name)
	}

	// Try to find plugin in local marketplace cache (from claude-plugins-official layout)
	if cachedDir := m.findCachedPlugin(name); cachedDir != "" {
		if err := copyDir(cachedDir, pluginDir); err != nil {
			os.RemoveAll(pluginDir)
			return fmt.Errorf("copy from cache: %w", err)
		}
		// Generate manifest.json from .claude-plugin/plugin.json if needed
		m.ensureManifest(pluginDir)

		if version == "" {
			version = readManifestVersion(pluginDir)
		}
		m.lockfile.Plugins[name] = LockEntry{
			Source:      source,
			Version:     version,
			InstalledAt: time.Now().Format(time.RFC3339),
			AutoUpdate:  true,
		}
		m.saveLockfile()
		return nil
	}

	// Validate source URL
	if !isValidSource(source) {
		return fmt.Errorf("invalid plugin source: %s", source)
	}

	// Clone the plugin repo
	if isGitURL(source) {
		args := []string{"clone", "--depth=1", "--quiet", source, pluginDir}
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(pluginDir) // cleanup on failure
			return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
		}
	} else {
		// Local path: copy directory
		if err := copyDir(source, pluginDir); err != nil {
			os.RemoveAll(pluginDir)
			return fmt.Errorf("copy plugin: %w", err)
		}
	}

	// Generate manifest.json from .claude-plugin/plugin.json if needed
	m.ensureManifest(pluginDir)

	// Validate manifest exists
	manifestPath := filepath.Join(pluginDir, "manifest.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		os.RemoveAll(pluginDir)
		return fmt.Errorf("plugin has no manifest.json or .claude-plugin/plugin.json")
	}

	// Get commit SHA for version lock
	sha := getGitSHA(pluginDir)
	if version == "" {
		version = readManifestVersion(pluginDir)
	}

	// Update lockfile
	m.lockfile.Plugins[name] = LockEntry{
		Source:      source,
		Version:     version,
		CommitSHA:   sha,
		InstalledAt: time.Now().Format(time.RFC3339),
		AutoUpdate:  true,
	}
	m.saveLockfile()

	return nil
}

// --- Update ---

// Update updates a single plugin to latest.
func (m *Marketplace) Update(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock, ok := m.lockfile.Plugins[name]
	if !ok {
		return fmt.Errorf("plugin %q not tracked (was it installed via marketplace?)", name)
	}

	pluginDir := filepath.Join(m.dir, name)
	if _, err := os.Stat(filepath.Join(pluginDir, ".git")); err != nil {
		return fmt.Errorf("plugin %q is not a git repo, cannot update", name)
	}

	// Pull latest
	cmd := exec.Command("git", "-C", pluginDir, "pull", "--ff-only", "--quiet")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Update lock
	lock.CommitSHA = getGitSHA(pluginDir)
	lock.Version = readManifestVersion(pluginDir)
	lock.UpdatedAt = time.Now().Format(time.RFC3339)
	m.lockfile.Plugins[name] = lock
	m.saveLockfile()

	return nil
}

// UpdateAll updates all plugins with auto_update=true.
func (m *Marketplace) UpdateAll() (updated []string, errs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, lock := range m.lockfile.Plugins {
		if !lock.AutoUpdate {
			continue
		}
		pluginDir := filepath.Join(m.dir, name)
		if _, err := os.Stat(filepath.Join(pluginDir, ".git")); err != nil {
			continue
		}

		cmd := exec.Command("git", "-C", pluginDir, "pull", "--ff-only", "--quiet")
		if out, err := cmd.CombinedOutput(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", name, strings.TrimSpace(string(out))))
			continue
		}

		newSHA := getGitSHA(pluginDir)
		if newSHA != lock.CommitSHA {
			lock.CommitSHA = newSHA
			lock.Version = readManifestVersion(pluginDir)
			lock.UpdatedAt = time.Now().Format(time.RFC3339)
			m.lockfile.Plugins[name] = lock
			updated = append(updated, name)
		}
	}

	if len(updated) > 0 {
		m.saveLockfile()
	}
	return
}

// --- Lockfile ---

func (m *Marketplace) lockfilePath() string {
	return filepath.Join(filepath.Dir(m.dir), "marketplace", "lock.json")
}

func (m *Marketplace) loadLockfile() {
	data, err := os.ReadFile(m.lockfilePath())
	if err != nil {
		return
	}
	json.Unmarshal(data, &m.lockfile)
	if m.lockfile.Plugins == nil {
		m.lockfile.Plugins = make(map[string]LockEntry)
	}
}

func (m *Marketplace) saveLockfile() {
	os.MkdirAll(filepath.Dir(m.lockfilePath()), 0755)
	data, _ := json.MarshalIndent(m.lockfile, "", "  ")
	os.WriteFile(m.lockfilePath(), data, 0644)
}

// LockInfo returns the lock entry for a plugin.
func (m *Marketplace) LockInfo(name string) (LockEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.lockfile.Plugins[name]
	return e, ok
}

// --- Helpers ---

func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	return s
}

func deriveNameFromURL(url string) string {
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimRight(url, "/")
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	return sanitizeName(name)
}

func isGitURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "ssh://")
}

func isValidSource(source string) bool {
	if source == "" {
		return false
	}
	// Block path traversal
	if strings.Contains(source, "..") {
		return false
	}
	// Must be a git URL or absolute/relative path
	if isGitURL(source) {
		return true
	}
	// Local path
	return filepath.IsAbs(source) || strings.HasPrefix(source, "./")
}

func getGitSHA(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readManifestVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "unknown"
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "unknown"
	}
	if m.Version == "" {
		return "0.0.0"
	}
	return m.Version
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

// findCachedPlugin looks for a plugin by name in the local marketplace cache
// (supports claude-plugins-official layout: plugins/<name>/ and external_plugins/<name>/).
func (m *Marketplace) findCachedPlugin(name string) string {
	// Scan all cached source repos
	cacheEntries, _ := os.ReadDir(m.cacheDir)
	for _, ce := range cacheEntries {
		if !ce.IsDir() {
			continue
		}
		repoDir := filepath.Join(m.cacheDir, ce.Name())
		// Check plugins/<name> and external_plugins/<name>
		for _, subdir := range []string{"plugins", "external_plugins"} {
			candidate := filepath.Join(repoDir, subdir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		// Also check root-level <name> (standard directory source)
		candidate := filepath.Join(repoDir, name)
		if _, err := os.Stat(filepath.Join(candidate, "manifest.json")); err == nil {
			return candidate
		}
		if _, err := os.Stat(filepath.Join(candidate, ".claude-plugin", "plugin.json")); err == nil {
			return candidate
		}
	}
	return ""
}

// ensureManifest generates a manifest.json from .claude-plugin/plugin.json if the
// plugin does not already have a manifest.json (Claude official format compatibility).
func (m *Marketplace) ensureManifest(pluginDir string) {
	manifestPath := filepath.Join(pluginDir, "manifest.json")
	if _, err := os.Stat(manifestPath); err == nil {
		return // already has manifest.json
	}

	cpPath := filepath.Join(pluginDir, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(cpPath)
	if err != nil {
		return
	}

	var cp claudePluginJSON
	if err := json.Unmarshal(data, &cp); err != nil {
		return
	}

	name := cp.Name
	if name == "" {
		name = filepath.Base(pluginDir)
	}
	version := cp.Version
	if version == "" {
		version = "0.0.0"
	}

	// Scan for skills, commands, agents
	var skills, commands []string
	if entries, err := os.ReadDir(filepath.Join(pluginDir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				skills = append(skills, e.Name())
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(pluginDir, "commands")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				commands = append(commands, e.Name())
			}
		}
	}

	manifest := Manifest{
		Name:        name,
		Version:     version,
		Description: cp.Description,
		Author:      cp.Author.Name,
		Skills:      skills,
		Commands:    commands,
	}
	out, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(manifestPath, out, 0644)
}
