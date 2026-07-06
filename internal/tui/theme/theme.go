// Package theme provides color themes for the cove TUI.
// Inspired by opencode's theme system but adapted for Bubble Tea v2.
package theme

import (
	"sync"
)

// Theme defines a set of named colors for the TUI.
// Each field is an ANSI color string usable by lipgloss.
type Theme struct {
	Name string

	// Core UI colors
	Primary   string // accent color (headers, status bar)
	Secondary string // secondary accent (user messages)
	Text      string // main text
	TextMuted string // dim/secondary text
	Background string // main background
	Error     string // error/danger
	Warning   string // warning
	Success   string // success/good

	// Markdown-specific colors
	CodeBG       string // code block background
	CodeFG       string // code block text
	CodeAccent   string // code inline accent
	Link         string // link text
	Blockquote   string // blockquote text
	Heading      string // heading text
	ListBullet   string // list bullet character

	// UI chrome
	Border       string // border color
	OverlayBG    string // overlay box background
	SelectedBG   string // selected item background
	SelectedFG   string // selected item text
	DimBG        string // dim/darker background for shadows
}

var (
	mu       sync.RWMutex
	current  = catppuccin()
)

// Current returns the active theme.
func Current() *Theme {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// SetTheme changes the active theme by name. Returns the theme and true if found.
func SetTheme(name string) (*Theme, bool) {
	t := byName(name)
	if t == nil {
		return current, false
	}
	mu.Lock()
	current = t
	mu.Unlock()
	return t, true
}

// Names returns all available theme names.
func Names() []string {
	return []string{
		"catppuccin",
		"dracula",
		"gruvbox",
		"onedark",
		"tokyonight",
	}
}

func byName(name string) *Theme {
	switch name {
	case "catppuccin":
		return catppuccin()
	case "dracula":
		return dracula()
	case "gruvbox":
		return gruvbox()
	case "onedark":
		return onedark()
	case "tokyonight":
		return tokyonight()
	default:
		return nil
	}
}
