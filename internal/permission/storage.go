package permission

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/liuzhixin405/cove/internal/fsatomic"
)

// FilePolicyStorage persists policy rules to a JSON file.
type FilePolicyStorage struct {
	path string
}

// NewFilePolicyStorage creates a file-backed policy store.
// Creates parent directories if needed.
func NewFilePolicyStorage(path string) (*FilePolicyStorage, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &FilePolicyStorage{path: path}, nil
}

// Path returns the file path of the store.
func (s *FilePolicyStorage) Path() string { return s.path }

// Load reads policy rules from the JSON file.
// Returns an empty slice if the file doesn't exist.
func (s *FilePolicyStorage) Load() ([]PolicyRule, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []PolicyRule{}, nil
		}
		return nil, err
	}
	var rules []PolicyRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// Save writes policy rules to the JSON file atomically, so a crash or a
// concurrent cove process never leaves a truncated file that would drop every
// persisted rule on the next load.
func (s *FilePolicyStorage) Save(rules []PolicyRule) error {
	if rules == nil {
		rules = []PolicyRule{}
	}
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(s.path, data, 0600)
}
