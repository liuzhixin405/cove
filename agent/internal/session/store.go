package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/agentgo/internal/api"
)

type Record struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Title     string        `json:"title"`
	Messages  []api.Message `json:"messages"`
	Model     string        `json:"model"`
	TokensIn  int           `json:"tokens_in"`
	TokensOut int           `json:"tokens_out"`
	Cost      float64       `json:"cost"`
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
	return filepath.Join(home, ".agentgo", "sessions"), nil
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
		// Read file but only decode metadata fields (skip Messages for speed)
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var meta struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
			Title     string    `json:"title"`
			Model     string    `json:"model"`
			TokensIn  int       `json:"tokens_in"`
			TokensOut int       `json:"tokens_out"`
			Cost      float64   `json:"cost"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		records = append(records, Record{
			ID:        meta.ID,
			CreatedAt: meta.CreatedAt,
			UpdatedAt: meta.UpdatedAt,
			Title:     meta.Title,
			Model:     meta.Model,
			TokensIn:  meta.TokensIn,
			TokensOut: meta.TokensOut,
			Cost:      meta.Cost,
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
