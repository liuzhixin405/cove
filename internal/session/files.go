package session

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/fsatomic"
)

// IsSessionFile reports whether name (a base name in the sessions directory)
// holds a session: <id>.jsonl, or the legacy <id>.json. index.json, atomic-
// write temp files and backups are not sessions.
//
// Everything that walks ~/.cove/sessions must use this (or ListSessionFiles)
// rather than matching "*.json": that pattern misses every .jsonl session and
// picks up index.json as if it were one.
func IsSessionFile(name string) bool {
	if name == indexFileName || name == reservedID+jsonlExt || fsatomic.IsTempName(name) {
		return false
	}
	ext := filepath.Ext(name)
	if ext != jsonlExt && ext != legacyExt {
		return false
	}
	return strings.TrimSuffix(name, ext) != ""
}

// SessionIDFromFile returns the session ID a session file name stands for:
// name without a trailing .jsonl or .json. Any other suffix is part of the ID
// (IDs may contain dots), so it is also safe on user input such as a file name
// copied out of ~/.cove/sessions.
func SessionIDFromFile(name string) string {
	name = strings.TrimSpace(name)
	for _, ext := range []string{jsonlExt, legacyExt} {
		if strings.HasSuffix(name, ext) {
			return strings.TrimSuffix(name, ext)
		}
	}
	return name
}

// reservedID is the base name of the sessions index (index.json). No session
// may use it: Load would read the index as a legacy session.
const reservedID = "index"

// ErrReservedID is returned for the reserved session ID "index".
var ErrReservedID = errors.New(`session id "index" is reserved for the sessions index (index.json), not a session`)

// ParseSessionID is SessionIDFromFile for user input (-r / --resume, /resume
// by file name): it also rejects the reserved ID "index", so "cove -r
// index.json" fails clearly instead of loading the index as a session.
func ParseSessionID(name string) (string, error) {
	id := SessionIDFromFile(name)
	if isReservedID(id) {
		return "", ErrReservedID
	}
	return id, nil
}

func isReservedID(id string) bool {
	return strings.EqualFold(fileKey(strings.TrimSpace(id)), reservedID)
}

// ListSessionFiles returns the base names of the session files in dir, sorted.
// A legacy <id>.json whose <id>.jsonl also exists (a migration interrupted
// before the old file was removed) is left out, so each session appears once.
func ListSessionFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	jsonl := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == jsonlExt {
			jsonl[SessionIDFromFile(e.Name())] = true
		}
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !IsSessionFile(name) {
			continue
		}
		if filepath.Ext(name) == legacyExt && jsonl[SessionIDFromFile(name)] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// NewStoreAt returns a Store for an explicit sessions directory, which must
// exist. NewStore uses ~/.cove/sessions.
func NewStoreAt(dir string) *Store { return &Store{dir: dir} }

// Replace rewrites r's session file in full and updates the index without
// restamping UpdatedAt. It is for maintenance passes over stored sessions
// (/history clean), which must not move a session to the top of the list.
func (s *Store) Replace(r *Record) error {
	return s.save(r, false, true)
}
