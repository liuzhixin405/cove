package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/fsatomic"
)

// newTestNotes builds a SessionNotes rooted in a temp project dir. HOME and
// USERPROFILE are redirected too so that nothing in this package can reach the
// developer's real ~/.cove.
func newTestNotes(t *testing.T) (*SessionNotes, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	projectDir := t.TempDir()
	return New(projectDir), projectDir
}

func TestNewPlacesNotesUnderProjectCoveDir(t *testing.T) {
	s, projectDir := newTestNotes(t)

	want := filepath.Join(projectDir, ".cove", "session_notes.md")
	if s.path != want {
		t.Errorf("path = %q, want %q", s.path, want)
	}
	info, err := os.Stat(filepath.Join(projectDir, ".cove"))
	if err != nil {
		t.Fatalf("New did not create the .cove dir: %v", err)
	}
	if !info.IsDir() {
		t.Error(".cove is not a directory")
	}
	if len(s.entries) != 0 {
		t.Errorf("new notes already holds %d entries", len(s.entries))
	}
}

func TestNewGlobalUsesHomeCoveDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := NewGlobal()
	want := filepath.Join(home, ".cove", "session_notes.md")
	if s.path != want {
		t.Fatalf("NewGlobal path = %q, want %q (it must honour the redirected home)", s.path, want)
	}

	s.AddDecision("全局笔记写入 home")
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("Flush did not write %s: %v", want, err)
	}
}

func TestAddCategoriesFlushAndLoadRoundTrip(t *testing.T) {
	s, projectDir := newTestNotes(t)

	s.AddDecision("采用 fsatomic 写入所有状态文件")
	s.AddTask("为 notes 包补充测试")
	s.AddDiscovery("Load 之前没有加锁")
	s.AddError("truncated JSON 无法恢复会话")
	s.Add("task", "第二个任务")

	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	path := filepath.Join(projectDir, ".cove", "session_notes.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read notes file: %v", err)
	}
	body := string(data)

	if !strings.HasPrefix(body, "# Session Notes\n\n") {
		t.Errorf("notes file does not start with the expected title: %.40q", body)
	}
	for _, want := range []string{
		"采用 fsatomic 写入所有状态文件",
		"为 notes 包补充测试",
		"Load 之前没有加锁",
		"truncated JSON 无法恢复会话",
		"第二个任务",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notes file is missing entry %q", want)
		}
	}

	// A second manager reading the same file must recover every entry with its
	// category, which is what resuming a session depends on.
	reloaded := New(projectDir)
	reloaded.Load()

	if len(reloaded.entries) != 5 {
		t.Fatalf("Load recovered %d entries, want 5", len(reloaded.entries))
	}
	byCategory := map[string][]string{}
	for _, e := range reloaded.entries {
		byCategory[e.Category] = append(byCategory[e.Category], e.Text)
	}
	wantByCategory := map[string][]string{
		"decision":  {"采用 fsatomic 写入所有状态文件"},
		"task":      {"为 notes 包补充测试", "第二个任务"},
		"discovery": {"Load 之前没有加锁"},
		"error":     {"truncated JSON 无法恢复会话"},
	}
	for cat, want := range wantByCategory {
		got := byCategory[cat]
		if len(got) != len(want) {
			t.Errorf("category %q loaded %v, want %v", cat, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("category %q entry %d = %q, want %q", cat, i, got[i], want[i])
			}
		}
	}
	if e := reloaded.entries[0]; e.Timestamp.IsZero() {
		t.Error("loaded entry has a zero Timestamp")
	}
}

func TestWriteToDiskGroupsCategoriesInFixedOrder(t *testing.T) {
	s, projectDir := newTestNotes(t)

	// Added out of order on purpose: the file must still be grouped
	// decision -> task -> discovery -> error.
	s.AddError("err-1")
	s.AddDiscovery("disc-1")
	s.AddTask("task-1")
	s.AddDecision("dec-1")
	s.AddTask("task-2")
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".cove", "session_notes.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)

	// titleCase(cat+"s") produces these exact headers ("Discoverys" included).
	headers := []string{"## Decisions", "## Tasks", "## Discoverys", "## Errors"}
	prev := -1
	for _, h := range headers {
		idx := strings.Index(body, h)
		if idx < 0 {
			t.Fatalf("header %q missing from notes file:\n%s", h, body)
		}
		if idx <= prev {
			t.Fatalf("header %q appears at %d, out of order (previous header ended at %d):\n%s", h, idx, prev, body)
		}
		prev = idx
	}

	// Entries stay in insertion order inside their group.
	tasks := body[strings.Index(body, "## Tasks"):strings.Index(body, "## Discoverys")]
	if i1, i2 := strings.Index(tasks, "task-1"), strings.Index(tasks, "task-2"); i1 < 0 || i2 < 0 || i1 > i2 {
		t.Errorf("tasks are not in insertion order:\n%s", tasks)
	}

	// A category with no entries must not emit an empty header.
	s2, dir2 := newTestNotes(t)
	s2.AddDecision("only a decision")
	if err := s2.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	data2, err := os.ReadFile(filepath.Join(dir2, ".cove", "session_notes.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, h := range []string{"## Tasks", "## Discoverys", "## Errors"} {
		if strings.Contains(string(data2), h) {
			t.Errorf("notes file emitted %q for an empty category:\n%s", h, data2)
		}
	}
}

func TestFlushWithoutModificationDoesNotWrite(t *testing.T) {
	s, projectDir := newTestNotes(t)
	path := filepath.Join(projectDir, ".cove", "session_notes.md")

	s.AddTask("first")
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("first Flush wrote nothing: %v", err)
	}

	// Deleting the file makes a second write unmistakable: if Flush honours the
	// modified flag the file stays gone.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Flush rewrote the file with no modifications (stat err = %v)", err)
	}

	// A new Add re-arms it.
	s.AddTask("second")
	if err := s.Flush(); err != nil {
		t.Fatalf("third Flush: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Flush after Add wrote nothing: %v", err)
	}
	if !strings.Contains(string(data), "second") {
		t.Errorf("notes file missing the new entry:\n%s", data)
	}
}

func TestFlushWithNoEntriesWritesNothing(t *testing.T) {
	s, projectDir := newTestNotes(t)
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	path := filepath.Join(projectDir, ".cove", "session_notes.md")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Flush with no entries created %s (stat err = %v)", path, err)
	}
}

func TestFlushLeavesNoTempFileBehind(t *testing.T) {
	s, projectDir := newTestNotes(t)
	for i := 0; i < 3; i++ {
		s.AddTask(fmt.Sprintf("entry-%d", i))
		if err := s.Flush(); err != nil {
			t.Fatalf("Flush #%d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(projectDir, ".cove"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if fsatomic.IsTempName(e.Name()) {
			t.Errorf("Flush left an atomic-write temp file behind: %s", e.Name())
		}
	}
	if len(names) != 1 || names[0] != "session_notes.md" {
		t.Errorf(".cove contains %v, want exactly [session_notes.md]", names)
	}
}

func TestContentShowsAtMostLast20Entries(t *testing.T) {
	s, _ := newTestNotes(t)
	for i := 1; i <= 25; i++ {
		s.AddTask(fmt.Sprintf("entry-%02d", i))
	}

	content := s.Content()
	if !strings.HasPrefix(content, "\n<session_notes>\n") || !strings.HasSuffix(content, "</session_notes>\n") {
		t.Fatalf("Content is not wrapped in the session_notes block:\n%q", content)
	}

	var lines []string
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(l, "- [") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 20 {
		t.Fatalf("Content listed %d entries, want the last 20", len(lines))
	}
	// The five oldest must have been dropped, the 20 newest kept in order.
	for i := 1; i <= 5; i++ {
		if strings.Contains(content, fmt.Sprintf("entry-%02d", i)) {
			t.Errorf("Content still contains dropped entry-%02d", i)
		}
	}
	for i, want := 6, 0; i <= 25; i, want = i+1, want+1 {
		expect := fmt.Sprintf("- [task] entry-%02d", i)
		if lines[want] != expect {
			t.Fatalf("Content line %d = %q, want %q", want, lines[want], expect)
		}
	}
}

func TestContentTagsEachEntryWithItsCategory(t *testing.T) {
	s, _ := newTestNotes(t)
	if got := s.Content(); got != "" {
		t.Errorf("Content with no entries = %q, want empty string", got)
	}

	s.AddDecision("d")
	s.AddError("e")
	s.AddDiscovery("v")
	want := "\n<session_notes>\n- [decision] d\n- [error] e\n- [discovery] v\n</session_notes>\n"
	if got := s.Content(); got != want {
		t.Errorf("Content =\n%q\nwant\n%q", got, want)
	}
}

func TestFlushClipsAt25KBOnRuneBoundary(t *testing.T) {
	const maxBytes = 25600
	const suffix = "\n... [truncated]\n"

	// The byte at offset 25600 has to land inside a 3-byte rune for the clip to
	// be interesting. Its alignment depends on how many bytes precede the
	// Chinese block, so shift that prefix by 0/1/2 bytes to cover all three
	// residues; at least one run puts the cut mid-rune.
	for pad := 0; pad < 3; pad++ {
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			s, projectDir := newTestNotes(t)
			if pad > 0 {
				s.AddTask(strings.Repeat("x", pad))
			}
			// 250 entries x 100 Chinese runes ~= 78KB, well past the cap.
			line := strings.Repeat("会话笔记内容", 17) // 102 runes, 306 bytes
			for i := 0; i < 250; i++ {
				s.AddTask(line)
			}
			if err := s.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(projectDir, ".cove", "session_notes.md"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			if !utf8.Valid(data) {
				t.Fatalf("notes file is not valid UTF-8 after the 25KB clip (len=%d); the cap cut a multi-byte rune in half", len(data))
			}
			if !strings.HasSuffix(string(data), suffix) {
				t.Fatalf("clipped notes file does not end with the truncation marker; tail = %q", tail(string(data), 40))
			}
			clipped := len(data) - len(suffix)
			// The rune-boundary walk-back can only lose 1-2 bytes.
			if clipped > maxBytes || clipped < maxBytes-2 {
				t.Fatalf("clipped body is %d bytes, want %d..%d", clipped, maxBytes-2, maxBytes)
			}
		})
	}
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func TestFlushDoesNotClipContentUnderTheCap(t *testing.T) {
	s, projectDir := newTestNotes(t)
	s.AddTask(strings.Repeat("短", 100))
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(projectDir, ".cove", "session_notes.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "[truncated]") {
		t.Errorf("small notes file was truncated:\n%s", data)
	}
	if !strings.Contains(string(data), strings.Repeat("短", 100)) {
		t.Error("entry text was altered by the write path")
	}
}

func TestLoadIgnoresMissingAndUnparseableFiles(t *testing.T) {
	s, projectDir := newTestNotes(t)

	// No file yet.
	s.Load()
	if len(s.entries) != 0 {
		t.Fatalf("Load of a missing file added %d entries", len(s.entries))
	}

	path := filepath.Join(projectDir, ".cove", "session_notes.md")
	body := strings.Join([]string{
		"# Session Notes",
		"",
		"_Last updated: 2026-01-01 00:00_",
		"",
		"## Decisions",
		"",
		"- [10:00] kept decision",
		"- no timestamp bracket at all",
		"-[10:00] missing space prefix",
		"## Unknown Heading",
		"",
		"- [11:00] falls back to task",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s.Load()
	if len(s.entries) != 2 {
		var got []string
		for _, e := range s.entries {
			got = append(got, e.Category+":"+e.Text)
		}
		t.Fatalf("Load parsed %v, want exactly the two well-formed entries", got)
	}
	if s.entries[0].Category != "decision" || s.entries[0].Text != "kept decision" {
		t.Errorf("entry 0 = %+v, want decision/\"kept decision\"", s.entries[0])
	}
	// An unrecognised "## " header leaves the previous category in place, so the
	// trailing entry is still attributed to "decision".
	if s.entries[1].Text != "falls back to task" {
		t.Errorf("entry 1 text = %q, want %q", s.entries[1].Text, "falls back to task")
	}
}

func TestLoadDefaultsToTaskWithoutAHeader(t *testing.T) {
	s, projectDir := newTestNotes(t)
	path := filepath.Join(projectDir, ".cove", "session_notes.md")
	if err := os.WriteFile(path, []byte("- [09:30] headerless entry\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.Load()
	if len(s.entries) != 1 {
		t.Fatalf("Load parsed %d entries, want 1", len(s.entries))
	}
	if s.entries[0].Category != "task" {
		t.Errorf("category = %q, want %q", s.entries[0].Category, "task")
	}
}

// TestLoadConcurrentWithAdd is the regression guard for the unsynchronised
// append Load used to do. Under -race an unlocked s.entries append here fails.
//
// The count is deterministic: the file is written once before the goroutines
// start and never rewritten, so every Load appends exactly the same number of
// entries.
func TestLoadConcurrentWithAdd(t *testing.T) {
	s, projectDir := newTestNotes(t)

	const fileEntries = 4
	for i := 0; i < fileEntries; i++ {
		s.AddDecision(fmt.Sprintf("seed-%d", i))
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".cove", "session_notes.md")); err != nil {
		t.Fatalf("seed file missing: %v", err)
	}

	const adds = 60
	const loads = 20

	var wg sync.WaitGroup
	for i := 0; i < adds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.AddTask(fmt.Sprintf("concurrent-%02d", i))
		}(i)
	}
	for i := 0; i < loads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Load()
			// Content also takes the lock; call it to exercise a reader racing
			// the writers.
			_ = s.Content()
		}()
	}
	wg.Wait()

	want := fileEntries + adds + loads*fileEntries
	if len(s.entries) != want {
		t.Fatalf("entries = %d, want %d (%d seed + %d added + %d x %d loaded)",
			len(s.entries), want, fileEntries, adds, loads, fileEntries)
	}

	// Every concurrent Add must be present exactly once.
	counts := map[string]int{}
	for _, e := range s.entries {
		counts[e.Text]++
	}
	for i := 0; i < adds; i++ {
		key := fmt.Sprintf("concurrent-%02d", i)
		if counts[key] != 1 {
			t.Fatalf("entry %q appears %d times, want 1 (a lost or duplicated append)", key, counts[key])
		}
	}
}

func TestTitleCase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"decisions", "Decisions"},
		{"a", "A"},
		{"Errors", "Errors"},
	} {
		if got := titleCase(tc.in); got != tc.want {
			t.Errorf("titleCase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
