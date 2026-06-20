package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

type Record struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Title     string        `json:"title"`
	Messages  []api.Message `json:"messages"`
	// MessageCount and Preview are list-only metadata populated by List().
	MessageCount int     `json:"-"`
	UserTurns    int     `json:"-"`
	Preview      string  `json:"-"`
	Model        string  `json:"model"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	Cost         float64 `json:"cost"`
}

type Store struct {
	dir string
}

func NewStore() (*Store, error) {
	dir, err := getSessionDir()
	if err != nil {
		return nil, err
	}
	os.MkdirAll(dir, 0700)
	return &Store{dir: dir}, nil
}

func getSessionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cove", "sessions"), nil
}

func (s *Store) Save(r *Record) error {
	r.UpdatedAt = time.Now()
	data, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(s.path(r.ID), data, 0600)
}

func (s *Store) Load(id string) (*Record, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	return &r, nil
}

func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		// Stream JSON decode to avoid large memory allocations for huge files
		f, err := os.Open(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var meta struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
			Title     string    `json:"title"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Model     string  `json:"model"`
			TokensIn  int     `json:"tokens_in"`
			TokensOut int     `json:"tokens_out"`
			Cost      float64 `json:"cost"`
		}
		err = json.NewDecoder(f).Decode(&meta)
		f.Close()
		if err != nil {
			continue
		}
		records = append(records, Record{
			ID:           meta.ID,
			CreatedAt:    meta.CreatedAt,
			UpdatedAt:    meta.UpdatedAt,
			Title:        meta.Title,
			MessageCount: len(meta.Messages),
			UserTurns:    countGenuineUserTurns(meta.Messages),
			Preview:      firstUserPreview(meta.Messages),
			Model:        meta.Model,
			TokensIn:     meta.TokensIn,
			TokensOut:    meta.TokensOut,
			Cost:         meta.Cost,
		})
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].UpdatedAt.After(records[j].UpdatedAt)
	})
	return records, nil
}

func (s *Store) path(id string) string {
	clean := filepath.Base(id)
	return filepath.Join(s.dir, clean+".json")
}

func firstUserPreview(messages []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) string {
	var fallback string
	for _, m := range messages {
		if m.Role != "user" || m.Content == "" {
			continue
		}
		text := compactPreview(m.Content, 50)
		if text == "" {
			continue
		}
		if !isLowSignalHistoryText(text) {
			return text
		}
		if fallback == "" {
			fallback = text
		}
	}
	return fallback
}

// countGenuineUserTurns counts only real user-authored turns, excluding the
// engine-injected synthetic prompts that are also stored under Role=="user"
// (e.g. the truncation-continuation nudge and circuit-breaker hints, both
// prefixed with "[system:"). The raw message count is misleading because it
// also includes assistant replies and tool-result messages.
func countGenuineUserTurns(messages []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) int {
	n := 0
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(m.Content), "[system:") {
			continue
		}
		n++
	}
	return n
}

func countToolMessages(messages []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) int {
	n := 0
	for _, m := range messages {
		if m.Role == "tool" {
			n++
		}
	}
	return n
}

func isLowSignalHistoryText(s string) bool {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return true
	}
	if len([]rune(v)) <= 2 {
		return true
	}
	noise := map[string]bool{
		"write":        true,
		"write a file": true,
		"read":         true,
		"read file":    true,
		"grep":         true,
		"继续":           true,
		"continue":     true,
		"你好":           true,
		"hi":           true,
		"hello":        true,
		"?":            true,
		"list":         true,
		"ls":           true,
		"l":            true,
		"bash":         true,
		"show":         true,
		"cat":          true,
	}
	if noise[v] {
		return true
	}
	if strings.HasPrefix(v, "/") {
		return true
	}
	// Filter out command line tools
	fields := strings.Fields(v)
	if len(fields) > 0 {
		first := fields[0]
		commonTools := map[string]bool{
			"cd": true, "pwd": true, "git": true, "grep": true, "find": true, "wc": true,
			"cat": true, "nano": true, "vim": true, "vi": true, "curl": true, "wget": true,
			"go": true, "python": true, "python3": true, "pip": true, "npm": true, "node": true,
			"yarn": true, "pnpm": true, "make": true, "docker": true, "powershell": true,
			"cmd": true, "dir": true, "ls": true, "rm": true, "cp": true, "mv": true,
			"mkdir": true, "touch": true, "ssh": true, "scp": true, "rsync": true,
		}
		if commonTools[first] {
			return true
		}
	}
	return false
}

func compactPreview(s string, maxLen int) string {
	s = trimWhitespaceLine(s)
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "..."
}

func trimWhitespaceLine(s string) string {
	out := make([]rune, 0, len(s))
	lastSpace := false
	for _, ch := range []rune(s) {
		if ch == '\r' || ch == '\n' || ch == '\t' || ch == ' ' {
			if !lastSpace {
				out = append(out, ' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		out = append(out, ch)
	}
	for len(out) > 0 && out[0] == ' ' {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}
