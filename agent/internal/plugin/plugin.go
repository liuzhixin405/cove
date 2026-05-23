package plugin

import (
	"encoding/json"
	"fmt"
	"os"
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
	plugins map[string]*Entry
	dir     string
	mu      sync.RWMutex
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
	m.scanPlugins()
}

func (m *Manager) scanPlugins() {
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m.loadPlugin(e.Name())
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
	_ = url
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

