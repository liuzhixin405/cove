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
	"github.com/liuzhixin405/cove/internal/fsatomic"
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
	// Cwd is the project directory the session was started in, normalized
	// with NormalizeProjectDir. Empty for sessions saved before it existed.
	Cwd string `json:"cwd,omitempty"`
}

type Store struct {
	dir string
}

func NewStore() (*Store, error) {
	dir, err := getSessionDir()
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(dir, 0700)
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
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		// The marshal error used to be discarded, and the partial output written
		// over the existing session — losing a conversation to a single
		// unserializable value in the record. Fail without touching the file.
		return fmt.Errorf("marshal session %s: %w", r.ID, err)
	}
	// Atomic replace: a crash or a full disk mid-write would otherwise truncate
	// the session file, and a truncated JSON record cannot be resumed at all.
	return fsatomic.WriteFile(s.path(r.ID), data, 0600)
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
				Role      string `json:"role"`
				Content   string `json:"content"`
				Synthetic bool   `json:"synthetic,omitempty"`
			} `json:"messages"`
			Model     string  `json:"model"`
			TokensIn  int     `json:"tokens_in"`
			TokensOut int     `json:"tokens_out"`
			Cost      float64 `json:"cost"`
			Cwd       string  `json:"cwd"`
		}
		err = json.NewDecoder(f).Decode(&meta)
		_ = f.Close()
		if err != nil {
			continue
		}
		// Skip test sessions (model == "test-model") so they don't pollute
		// the user's history list. Tests create real session files but those
		// are noise, not genuine user conversations.
		if meta.Model == "test-model" {
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
			Cwd:          meta.Cwd,
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
	Role      string `json:"role"`
	Content   string `json:"content"`
	Synthetic bool   `json:"synthetic,omitempty"`
}) string {
	for _, m := range messages {
		if m.Role == "user" && !m.Synthetic && strings.TrimSpace(m.Content) != "" {
			if !looksSyntheticContent(m.Content) {
				return compactPreview(m.Content, 50)
			}
		}
	}
	return ""
}

func looksSyntheticContent(c string) bool {
	c = strings.TrimSpace(c)
	knownPrefixes := []string{
		"[system:", "[Conversation Summary]",
		"[系统检测到重复操作循环]", "[Context truncated",
		"[用户指引]", "[Continue the task", "[会话摘要]",
	}
	for _, p := range knownPrefixes {
		if strings.HasPrefix(c, p) || strings.EqualFold(c, p) {
			return true
		}
	}
	return false
}

// countGenuineUserTurns counts only real user-authored turns, excluding the
// engine-injected synthetic prompts that are also stored under Role=="user"
// (e.g. the truncation-continuation nudge and circuit-breaker hints, both
// prefixed with "[system:"). The raw message count is misleading because it
// also includes assistant replies and tool-result messages.
func countGenuineUserTurns(messages []struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Synthetic bool   `json:"synthetic,omitempty"`
}) int {
	n := 0
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		if m.Synthetic {
			continue
		}
		// Older sessions (saved before the Synthetic flag, or via a code path that
		// forgot to set it) store engine-injected prompts under Role=="user" with a
		// "[system:" / summary prefix. Exclude those too so the genuine-turn count —
		// and the Ctrl+R history filter that depends on it — isn't fooled.
		if looksSyntheticContent(m.Content) {
			continue
		}
		n++
	}
	return n
}

func countToolMessages(messages []struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Synthetic bool   `json:"synthetic,omitempty"`
}) int {
	n := 0
	for _, m := range messages {
		if m.Role == "tool" {
			n++
		}
	}
	return n
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
	for _, ch := range s {
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
