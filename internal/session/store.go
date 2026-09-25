package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/fsatomic"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/memory"
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

// On-disk layout of ~/.cove/sessions:
//
//	<id>.jsonl   one session: line 1 is a fileMeta, every further line is one
//	             api.Message. Save only appends the messages that are new.
//	index.json   list metadata for every session (title, cwd, turns, preview,
//	             updated_at, tokens, cost), so List never decodes a message.
//	<id>.json    the old whole-file format. Still loaded; the next Save of that
//	             session migrates it to <id>.jsonl and removes it.
//
// The metadata that changes on every save (updated_at, title, tokens, cost)
// lives in index.json; the first line of a .jsonl carries the values of the
// last full rewrite and is the fallback when the index is lost.
const (
	indexFileName   = "index.json"
	jsonlExt        = ".jsonl"
	legacyExt       = ".json"
	sessionFormat   = "cove-session"
	sessionVersion  = 1
	indexVersion    = 1
	previewMaxRunes = 50
)

// fileMeta is the first line of a .jsonl session file.
type fileMeta struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Title     string    `json:"title"`
	Model     string    `json:"model"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	Cost      float64   `json:"cost"`
	Cwd       string    `json:"cwd,omitempty"`
}

// indexEntry is one session's list metadata. File, Size and ModTime identify
// the file state it was computed from: when the file no longer matches (a
// crash between append and index update, an external edit) List rescans it.
type indexEntry struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Cwd          string    `json:"cwd,omitempty"`
	Turns        int       `json:"turns"`
	MessageCount int       `json:"message_count"`
	Preview      string    `json:"preview,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Model        string    `json:"model"`
	TokensIn     int       `json:"tokens_in"`
	TokensOut    int       `json:"tokens_out"`
	Cost         float64   `json:"cost"`
	File         string    `json:"file"`
	Size         int64     `json:"size"`
	ModTime      int64     `json:"mod_time"`
}

// writeIndexFile replaces index.json; a variable so tests can make it fail.
var writeIndexFile = func(path string, data []byte) error {
	return fsatomic.WriteFile(path, data, 0600)
}

// fileDecodes counts session files List had to decode because the index did
// not cover them; tests use it to prove the index is what answers List.
var fileDecodes atomic.Int64

type indexFile struct {
	Version int `json:"version"`
	// Sessions is keyed by the file's base name (the sanitized ID).
	Sessions map[string]*indexEntry `json:"sessions"`
}

// msgPrint identifies a persisted message cheaply. Strings share their
// backing array with the engine's history, so comparing an unchanged message
// is a pointer compare, not a byte compare.
type msgPrint struct {
	role, content, reasoning, toolCallID, name string
	synthetic                                  bool
	parts, calls, thinking                     int
	firstCallID, firstPartData                 string
}

func printOf(m api.Message) msgPrint {
	p := msgPrint{
		role: m.Role, content: m.Content, reasoning: m.ReasoningContent,
		toolCallID: m.ToolCallID, name: m.Name, synthetic: m.Synthetic,
		parts: len(m.Parts), calls: len(m.ToolCalls), thinking: len(m.ThinkingBlocks),
	}
	if len(m.ToolCalls) > 0 {
		p.firstCallID = m.ToolCalls[0].ID
	}
	if len(m.Parts) > 0 {
		p.firstPartData = m.Parts[0].Data
	}
	return p
}

// persistedState is what this Store knows is on disk for one session file.
type persistedState struct {
	prints []msgPrint
	size   int64
	// meta is the file's first line as last written, and appends counts the
	// appends since then; together they decide when the line is refreshed.
	meta    fileMeta
	appends int
}

// metaRefreshEvery bounds how stale the first line's tokens and cost may get:
// they change on every turn, and rewriting the file for each change would undo
// the append-only format, so they ride along with every metaRefreshEvery-th
// append. A title, model or cwd change refreshes the line on the next save.
// index.json always carries the current values; the line is its fallback.
const metaRefreshEvery = 16

// metaStale reports whether the first line should be rewritten with r's
// metadata as part of this save.
func (st *persistedState) metaStale(r *Record) bool {
	m := metaOf(r)
	if m.Title != st.meta.Title || m.Model != st.meta.Model || m.Cwd != st.meta.Cwd {
		return true
	}
	if m.TokensIn != st.meta.TokensIn || m.TokensOut != st.meta.TokensOut || m.Cost != st.meta.Cost {
		return st.appends+1 >= metaRefreshEvery
	}
	return false
}

type Store struct {
	dir string

	mu sync.Mutex
	// persisted tracks, per file base name, which messages are already in the
	// .jsonl file, so Save can append only the new ones. A session without an
	// entry (never saved or loaded by this Store, or loaded from a damaged or
	// legacy file) is rewritten in full on its next Save.
	persisted map[string]*persistedState
	// lastAutoPrune throttles AutoPrune.
	lastAutoPrune time.Time
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

// Save persists r. Messages already written by an earlier Save are not
// rewritten: only the new ones are appended to <id>.jsonl, and the list
// metadata is updated in index.json. When the history is not an extension of
// what is on disk (compaction, /clear, a file changed behind our back) the
// file is rewritten atomically instead.
func (s *Store) Save(r *Record) error {
	return s.save(r, true, false)
}

func (s *Store) save(r *Record, stamp, forceRewrite bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if stamp {
		r.UpdatedAt = time.Now()
	}
	key := fileKey(r.ID)
	path := s.path(r.ID)

	var err error
	if st := s.appendableState(key, path, r.Messages); st != nil && !forceRewrite && !st.metaStale(r) {
		err = s.appendMessages(r, key, path, st)
	} else {
		err = s.rewrite(r, key, path)
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat session %s: %w", r.ID, err)
	}
	s.state(key).size = info.Size()
	// The messages are on disk; the index is a cache List rebuilds from the
	// file when it does not match. index.json is shared by every cove process
	// and replacing it can fail on Windows while another one reads it, so a
	// failed update is logged, not returned: the caller's save did succeed.
	if err := s.updateIndex(func(idx *indexFile) {
		idx.Sessions[key] = entryFor(r, filepath.Base(path), info)
	}); err != nil {
		log.Warnf("session index update for %s failed (List will rebuild it): %v", r.ID, err)
	}
	return nil
}

// appendableState returns the persisted state for key when msgs extends it
// and the file is still exactly as this Store left it; nil means rewrite.
func (s *Store) appendableState(key, path string, msgs []api.Message) *persistedState {
	st := s.persisted[key]
	if st == nil || len(msgs) < len(st.prints) {
		return nil
	}
	for i, p := range st.prints {
		if printOf(msgs[i]) != p {
			return nil
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != st.size {
		return nil
	}
	return st
}

func (s *Store) state(key string) *persistedState {
	if s.persisted == nil {
		s.persisted = map[string]*persistedState{}
	}
	st := s.persisted[key]
	if st == nil {
		st = &persistedState{}
		s.persisted[key] = st
	}
	return st
}

func (s *Store) appendMessages(r *Record, key, path string, st *persistedState) error {
	fresh := r.Messages[len(st.prints):]
	if len(fresh) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, m := range fresh {
		line, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal session %s: %w", r.ID, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("append session %s: %w", r.ID, err)
	}
	_, werr := f.Write(buf.Bytes())
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		// What reached the disk is unknown; the next Save rewrites the file.
		delete(s.persisted, key)
		return fmt.Errorf("append session %s: %w", r.ID, err)
	}
	for _, m := range fresh {
		st.prints = append(st.prints, printOf(m))
	}
	st.appends++
	return nil
}

// rewrite replaces the session file with the full record, atomically, and
// removes a legacy <id>.json it supersedes.
func (s *Store) rewrite(r *Record, key, path string) error {
	var buf bytes.Buffer
	meta, err := json.Marshal(metaOf(r))
	if err != nil {
		return fmt.Errorf("marshal session %s: %w", r.ID, err)
	}
	buf.Write(meta)
	buf.WriteByte('\n')
	for _, m := range r.Messages {
		line, err := json.Marshal(m)
		if err != nil {
			// Fail without touching the file: a single unserializable value
			// must not cost the conversation already on disk.
			return fmt.Errorf("marshal session %s: %w", r.ID, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	delete(s.persisted, key)
	// Atomic replace: a crash or a full disk mid-write would otherwise truncate
	// the session file.
	if err := fsatomic.WriteFile(path, buf.Bytes(), 0600); err != nil {
		return err
	}
	if err := os.Remove(s.legacyPath(r.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove migrated session %s: %w", r.ID, err)
	}
	st := s.state(key)
	st.prints = make([]msgPrint, len(r.Messages))
	for i, m := range r.Messages {
		st.prints[i] = printOf(m)
	}
	st.meta, st.appends = metaOf(r), 0
	return nil
}

func (s *Store) Load(id string) (*Record, error) {
	if isReservedID(id) {
		return nil, ErrReservedID
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := fileKey(id)
	path := s.path(id)
	data, err := os.ReadFile(path)
	if err == nil {
		r, clean, perr := parseJSONL(data)
		if perr != nil {
			return nil, fmt.Errorf("parse session: %w", perr)
		}
		onDisk := metaOf(r) // the first line, before the index overlays it
		if idx, ierr := s.readIndex(); ierr == nil {
			if e := idx.Sessions[key]; e != nil && e.File == filepath.Base(path) {
				overlayEntry(r, e)
			}
		}
		if clean {
			st := s.state(key)
			st.size = int64(len(data))
			st.prints = make([]msgPrint, len(r.Messages))
			for i, m := range r.Messages {
				st.prints[i] = printOf(m)
			}
			st.meta, st.appends = onDisk, 0
		} else {
			// Damaged lines were skipped; the next Save rewrites the file
			// without them instead of appending after the damage.
			delete(s.persisted, key)
		}
		return r, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}

	data, err = os.ReadFile(s.legacyPath(id))
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	// No persisted state: the next Save writes <id>.jsonl in full and removes
	// the legacy file.
	delete(s.persisted, key)
	return &r, nil
}

// parseJSONL decodes a .jsonl session. clean is false when a line had to be
// skipped (a torn final line after a crash, or a damaged one).
func parseJSONL(data []byte) (*Record, bool, error) {
	rd := bufio.NewReader(bytes.NewReader(data))
	first, err := rd.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	var meta fileMeta
	if err := json.Unmarshal(bytes.TrimSpace(first), &meta); err != nil {
		return nil, false, err
	}
	if meta.Format != sessionFormat {
		return nil, false, fmt.Errorf("not a cove session file (format %q)", meta.Format)
	}
	r := recordOf(meta)
	clean := len(first) > 0 && first[len(first)-1] == '\n'
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var m api.Message
			if uerr := json.Unmarshal(line, &m); uerr != nil {
				clean = false
			} else {
				r.Messages = append(r.Messages, m)
			}
			if errors.Is(err, io.EOF) {
				// A last line without its newline was cut off mid-write.
				clean = false
			}
		}
		if err != nil {
			break
		}
	}
	return r, clean, nil
}

func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	entries, err := s.scanLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, e := range entries {
		// Skip test sessions (model == "test-model") so they don't pollute
		// the user's history list. Tests create real session files but those
		// are noise, not genuine user conversations.
		if e.Model == "test-model" {
			continue
		}
		records = append(records, Record{
			ID:           e.ID,
			CreatedAt:    e.CreatedAt,
			UpdatedAt:    e.UpdatedAt,
			Title:        e.Title,
			MessageCount: e.MessageCount,
			UserTurns:    e.Turns,
			Preview:      e.Preview,
			Model:        e.Model,
			TokensIn:     e.TokensIn,
			TokensOut:    e.TokensOut,
			Cost:         e.Cost,
			Cwd:          e.Cwd,
		})
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].UpdatedAt.After(records[j].UpdatedAt)
	})
	return records, nil
}

// Prune deletes all but the keep most recently updated sessions of each project
// directory (Record.Cwd) and returns
// how many it removed. keep <= 0 is a no-op rather than "delete everything".
//
// protect lists session IDs that are never deleted — the session in use, which
// may be old by UpdatedAt yet about to be saved again. Protected sessions do
// not count against keep. The listing, the choice and the deletion all happen
// under the store's lock, so a Save through this Store cannot land in between.
func (s *Store) Prune(keep int, protect ...string) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.scanLocked()
	if err != nil {
		return 0, err
	}
	protected := map[string]bool{}
	for _, id := range protect {
		protected[fileKey(id)] = true
	}
	var candidates []*indexEntry
	for _, e := range entries {
		if !protected[SessionIDFromFile(e.File)] {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) <= keep {
		return 0, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
	})
	// keep counts per project directory: one busy project must not push
	// every other project's sessions out. Sessions with no recorded
	// directory form one group of their own.
	perProject := map[string]int{}
	groupOf := map[string]string{} // cwd -> group, so each directory is resolved once
	var excess []*indexEntry
	for _, e := range candidates {
		k, ok := groupOf[e.Cwd]
		if !ok {
			k = projectGroup(e.Cwd)
			groupOf[e.Cwd] = k
		}
		perProject[k]++
		if perProject[k] > keep {
			excess = append(excess, e)
		}
	}

	removed := 0
	var errs []error
	var gone []string
	for _, e := range excess {
		key := SessionIDFromFile(e.File)
		failed := false
		for _, name := range []string{key + jsonlExt, key + legacyExt} {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
				failed = true
			}
		}
		if failed {
			continue
		}
		delete(s.persisted, key)
		gone = append(gone, key)
		removed++
	}
	if err := s.updateIndex(func(idx *indexFile) {
		for _, key := range gone {
			delete(idx.Sessions, key)
		}
	}); err != nil {
		errs = append(errs, err)
	}
	return removed, errors.Join(errs...)
}

// projectGroup is the per-project prune bucket for a session directory: the
// key of its repository root (memory.ProjectRoot), as the per-project memory
// directory uses, so sessions started in subdirectories of one repository
// count together; "" for sessions with no directory.
func projectGroup(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	return config.ProjectKey(memory.ProjectRoot(cwd))
}

// autoPruneInterval is how often AutoPrune actually prunes: listing the
// directory every turn is wasted work once it has been trimmed.
const autoPruneInterval = 10 * time.Minute

// AutoPrune is Prune for the end of a turn: it keeps the keep most recent
// sessions plus the protected ones (the session in use), and does the work at
// most once per autoPruneInterval per Store. keep <= 0 (max_sessions set to 0
// or below) disables it. It returns how many sessions were removed.
func (s *Store) AutoPrune(keep int, protect ...string) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	if !s.lastAutoPrune.IsZero() && time.Since(s.lastAutoPrune) < autoPruneInterval {
		s.mu.Unlock()
		return 0, nil
	}
	s.lastAutoPrune = time.Now()
	s.mu.Unlock()
	n, err := s.Prune(keep, protect...)
	if n > 0 {
		log.Debugf("session auto-prune removed %d sessions (keeping %d)", n, keep)
	}
	return n, err
}

// scanLocked returns the list metadata of every readable session in the
// directory, taking it from index.json where the index still matches the file
// and rescanning (and re-indexing) the files where it does not. s.mu is held.
func (s *Store) scanLocked() ([]*indexEntry, error) {
	dirEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	idx, _ := s.readIndex() // a missing or damaged index is rebuilt below
	dirty := false
	hasJSONL := map[string]bool{}
	for _, de := range dirEntries {
		if !de.IsDir() && filepath.Ext(de.Name()) == jsonlExt {
			hasJSONL[strings.TrimSuffix(de.Name(), jsonlExt)] = true
		}
	}

	seen := map[string]bool{}
	var out []*indexEntry
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !IsSessionFile(name) {
			continue
		}
		ext := filepath.Ext(name)
		key := strings.TrimSuffix(name, ext)
		if ext == legacyExt && hasJSONL[key] {
			continue // superseded by a migrated .jsonl
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := idx.Sessions[key]
		if e == nil || e.File != name || e.Size != info.Size() || e.ModTime != info.ModTime().UnixNano() {
			fresh, err := s.indexFileAt(filepath.Join(s.dir, name), info, e)
			if err != nil {
				continue // unreadable or corrupt: not listed, not indexed
			}
			idx.Sessions[key] = fresh
			e = fresh
			dirty = true
		}
		seen[key] = true
		out = append(out, e)
	}
	for key := range idx.Sessions {
		if !seen[key] {
			delete(idx.Sessions, key)
			dirty = true
		}
	}
	if dirty {
		_ = s.writeIndex(idx) // best effort: the next List rescans again
	}
	return out, nil
}

// listMsg is the part of a message List needs. It is an alias so the helpers
// below accept the anonymous struct slices the tests build.
type listMsg = struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Synthetic bool   `json:"synthetic,omitempty"`
}

// indexFileAt computes the index entry for one session file by decoding it.
// prev, when set, is the entry an earlier save wrote; its metadata is newer
// than a .jsonl file's first line and is kept.
func (s *Store) indexFileAt(path string, info fs.FileInfo, prev *indexEntry) (*indexEntry, error) {
	fileDecodes.Add(1)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var meta fileMeta
	var msgs []listMsg
	if filepath.Ext(path) == jsonlExt {
		rd := bufio.NewReader(f)
		first, err := rd.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if err := json.Unmarshal(bytes.TrimSpace(first), &meta); err != nil {
			return nil, err
		}
		if meta.Format != sessionFormat {
			return nil, fmt.Errorf("not a cove session file")
		}
		for {
			line, err := rd.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				var m listMsg
				if json.Unmarshal(line, &m) == nil {
					msgs = append(msgs, m)
				}
			}
			if err != nil {
				break
			}
		}
	} else {
		var legacy struct {
			fileMeta
			Messages []listMsg `json:"messages"`
		}
		if err := json.NewDecoder(f).Decode(&legacy); err != nil {
			return nil, err
		}
		meta = legacy.fileMeta
		msgs = legacy.Messages
	}

	e := &indexEntry{
		ID: meta.ID, Title: meta.Title, Cwd: meta.Cwd,
		CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt,
		Model: meta.Model, TokensIn: meta.TokensIn, TokensOut: meta.TokensOut, Cost: meta.Cost,
	}
	if prev != nil && prev.File == filepath.Base(path) && !prev.UpdatedAt.Before(meta.UpdatedAt) {
		e.ID, e.Title, e.Cwd = prev.ID, prev.Title, prev.Cwd
		e.UpdatedAt, e.Model = prev.UpdatedAt, prev.Model
		e.TokensIn, e.TokensOut, e.Cost = prev.TokensIn, prev.TokensOut, prev.Cost
	}
	e.MessageCount = len(msgs)
	e.Turns = countGenuineUserTurns(msgs)
	e.Preview = firstUserPreview(msgs)
	e.File = filepath.Base(path)
	e.Size = info.Size()
	e.ModTime = info.ModTime().UnixNano()
	return e, nil
}

func (s *Store) readIndex() (*indexFile, error) {
	idx := &indexFile{Version: indexVersion, Sessions: map[string]*indexEntry{}}
	data, err := os.ReadFile(filepath.Join(s.dir, indexFileName))
	if err != nil {
		return idx, err
	}
	var disk indexFile
	if err := json.Unmarshal(data, &disk); err != nil {
		return idx, err
	}
	for k, e := range disk.Sessions {
		if e != nil {
			idx.Sessions[k] = e
		}
	}
	return idx, nil
}

func (s *Store) writeIndex(idx *indexFile) error {
	idx.Version = indexVersion
	data, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("marshal session index: %w", err)
	}
	return writeIndexFile(filepath.Join(s.dir, indexFileName), data)
}

// updateIndex re-reads index.json, applies change and writes it back. The
// index is re-read rather than cached so that two cove processes sharing the
// directory do not drop each other's entries.
func (s *Store) updateIndex(change func(*indexFile)) error {
	idx, _ := s.readIndex()
	change(idx)
	return s.writeIndex(idx)
}

func entryFor(r *Record, file string, info fs.FileInfo) *indexEntry {
	msgs := make([]listMsg, len(r.Messages))
	for i, m := range r.Messages {
		msgs[i] = listMsg{Role: m.Role, Content: m.Content, Synthetic: m.Synthetic}
	}
	return &indexEntry{
		ID: r.ID, Title: r.Title, Cwd: r.Cwd,
		Turns: countGenuineUserTurns(msgs), MessageCount: len(msgs), Preview: firstUserPreview(msgs),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		Model: r.Model, TokensIn: r.TokensIn, TokensOut: r.TokensOut, Cost: r.Cost,
		File: file, Size: info.Size(), ModTime: info.ModTime().UnixNano(),
	}
}

func metaOf(r *Record) fileMeta {
	return fileMeta{
		Format: sessionFormat, Version: sessionVersion,
		ID: r.ID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Title: r.Title,
		Model: r.Model, TokensIn: r.TokensIn, TokensOut: r.TokensOut, Cost: r.Cost, Cwd: r.Cwd,
	}
}

func recordOf(m fileMeta) *Record {
	return &Record{
		ID: m.ID, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Title: m.Title,
		Model: m.Model, TokensIn: m.TokensIn, TokensOut: m.TokensOut, Cost: m.Cost, Cwd: m.Cwd,
	}
}

// overlayEntry applies the index's metadata, which every Save updates, over
// the .jsonl first line, which only a full rewrite does.
func overlayEntry(r *Record, e *indexEntry) {
	if e.UpdatedAt.Before(r.UpdatedAt) {
		return
	}
	r.UpdatedAt = e.UpdatedAt
	r.Title = e.Title
	r.Model = e.Model
	r.TokensIn, r.TokensOut, r.Cost = e.TokensIn, e.TokensOut, e.Cost
	if e.Cwd != "" {
		r.Cwd = e.Cwd
	}
}

// fileKey is the file base name for a session ID; filepath.Base keeps a
// hostile ID ("../../x") inside the store directory.
func fileKey(id string) string { return filepath.Base(id) }

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, fileKey(id)+jsonlExt)
}

func (s *Store) legacyPath(id string) string {
	return filepath.Join(s.dir, fileKey(id)+legacyExt)
}

func firstUserPreview(messages []listMsg) string {
	for _, m := range messages {
		if m.Role == "user" && !m.Synthetic && strings.TrimSpace(m.Content) != "" {
			if !looksSyntheticContent(m.Content) {
				return compactPreview(m.Content, previewMaxRunes)
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
func countGenuineUserTurns(messages []listMsg) int {
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

func countToolMessages(messages []listMsg) int {
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
