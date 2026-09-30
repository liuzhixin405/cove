package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// T2 会话存储：磁盘状态 × 操作（List+Load / Save / Delete / Prune / AutoPrune）。
//
// Every (state, op) pair runs on a fresh directory prepared as another cove
// process (or a crash, or the user) left it; the Store under test has never
// seen it, as after a restart. After the op checkStoreInvariants runs (no
// temp residue, only session files, List agrees with index.json).

// diskState prepares s.dir and states what the store must make of it. The
// session every op works on is "a".
type diskState struct {
	name  string
	setup func(t *testing.T, s *Store)
	// list is the IDs List must return (any order).
	list []string
	// loadA is Load("a")'s outcome and, when ok, its message contents.
	loadA    outcome
	loadMsgs []string
	// saveA is the outcome of Load("a") (a fresh record if that fails) +
	// one appended turn + Save. When ok, Load must then return loadMsgs
	// plus that turn.
	saveA outcome
	// delA is Delete("a")'s outcome; when ok, neither a.jsonl nor a.json
	// is left.
	delA outcome
	// pruned is how many sessions Prune(1, "a") removes; "a" always stays.
	pruned int
	// extra holds state-specific checks, run after the op they are named
	// for ("list", "save", "delete", "prune", "autoprune").
	extra map[string]func(t *testing.T, s *Store, ctx string, err error)
	// skip maps an op to a "bug: ..." reason.
	skip   map[string]string
	onlyOS string // "windows": the state is not reproducible elsewhere
	note   string
}

var diskBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// plantAt writes session id with the given turns, updated hours after
// diskBase.
func plantAt(t *testing.T, s *Store, id string, hours int, turns ...string) {
	t.Helper()
	plantSession(t, s.dir, tableRecord(id, turns...), diskBase.Add(time.Duration(hours)*time.Hour))
}

func writeLegacy(t *testing.T, s *Store, id string, turns ...string) {
	t.Helper()
	r := tableRecord(id, turns...)
	r.UpdatedAt = diskBase
	writeRawSession(t, s.dir, id+legacyExt, recordJSON(t, *r))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var diskStates = []diskState{
	{
		name: "空目录", setup: func(t *testing.T, s *Store) {},
		list: nil, loadA: wantNotExist, saveA: wantOK, delA: wantNotExist, pruned: 0,
		extra: map[string]func(*testing.T, *Store, string, error){
			"list": func(t *testing.T, s *Store, ctx string, _ error) {
				if fileExists(filepath.Join(s.dir, indexFileName)) {
					t.Errorf("%s: 空目录的 List 写出了 index.json (want no write when nothing changed)", ctx)
				}
			},
		},
	},
	{
		name: "正常", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			plantAt(t, s, "b", 1, "b1")
			plantAt(t, s, "c", 2, "c1")
		},
		list: []string{"a", "b", "c"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantOK, pruned: 1,
		extra: map[string]func(*testing.T, *Store, string, error){
			// (Index-only answering is pinned by TestListDoesNotDecodeMessageBodies;
			// fileDecodes is global, so parallel subtests cannot use it.)
			"save": func(t *testing.T, s *Store, ctx string, _ error) {
				if fileExists(s.archiveDir("a")) {
					t.Errorf("%s: 只追加的保存产生了归档 (want none)", ctx)
				}
			},
			"prune": func(t *testing.T, s *Store, ctx string, _ error) {
				if fileExists(s.path("b")) || !fileExists(s.path("c")) {
					t.Errorf("%s: got b=%v c=%v, want the older b pruned and c kept", ctx, fileExists(s.path("b")), fileExists(s.path("c")))
				}
			},
		},
	},
	{
		name: "撕裂的最后一行", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1", "a2 torn")
			p := s.path("a")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			// Cut the last line mid-way, as a crash during an append does.
			if err := os.WriteFile(p, data[:len(data)-12], 0o600); err != nil {
				t.Fatal(err)
			}
			plantAt(t, s, "b", 1, "b1")
		},
		list: []string{"a", "b"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantOK, pruned: 0,
		note: "撕裂行被跳过；下一次 Save 重写而不是接在损坏处后面追加",
		extra: map[string]func(*testing.T, *Store, string, error){
			"list": func(t *testing.T, s *Store, ctx string, _ error) {
				idx, _ := s.readIndex()
				if e := idx.Sessions["a"]; e == nil || e.MessageCount != 1 {
					t.Errorf("%s: 索引条目 got %+v, want repaired with message_count=1", ctx, e)
				}
			},
			"save": func(t *testing.T, s *Store, ctx string, _ error) {
				data, _ := os.ReadFile(s.path("a"))
				if _, clean, err := parseJSONL(data); err != nil || !clean {
					t.Errorf("%s: 保存后文件 got clean=%v err=%v, want a clean rewrite:\n%s", ctx, clean, err, data)
				}
			},
		},
	},
	{
		name: "legacy与jsonl并存", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			writeLegacy(t, s, "a", "legacy turn")
		},
		list: []string{"a"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantOK, pruned: 0,
		note: "迁移中断：.jsonl 优先，同一会话只列一次",
	},
	{
		name: "只有legacy", setup: func(t *testing.T, s *Store) {
			writeLegacy(t, s, "a", "l1")
			plantAt(t, s, "b", 1, "b1")
		},
		list: []string{"a", "b"}, loadA: wantOK, loadMsgs: []string{"l1"},
		saveA: wantOK, delA: wantOK, pruned: 0,
		note: "下一次 Save 迁移为 .jsonl 并删除 .json",
		extra: map[string]func(*testing.T, *Store, string, error){
			"save": func(t *testing.T, s *Store, ctx string, _ error) {
				if fileExists(s.legacyPath("a")) || !fileExists(s.path("a")) {
					t.Errorf("%s: got a.json=%v a.jsonl=%v, want migrated (a.jsonl only)", ctx, fileExists(s.legacyPath("a")), fileExists(s.path("a")))
				}
			},
		},
	},
	{
		name: "索引列出不存在的文件", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			plantAt(t, s, "ghost", 1, "g1")
			if err := os.Remove(s.path("ghost")); err != nil {
				t.Fatal(err)
			}
		},
		list: []string{"a"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantOK, pruned: 0,
		extra: map[string]func(*testing.T, *Store, string, error){
			"list": func(t *testing.T, s *Store, ctx string, _ error) {
				if idx, _ := s.readIndex(); idx.Sessions["ghost"] != nil {
					t.Errorf("%s: List 后索引仍有 ghost (want removed)", ctx)
				}
			},
			"delete": func(t *testing.T, s *Store, ctx string, _ error) {
				s2 := NewStoreAt(s.dir)
				plantAt(t, s2, "ghost2", 3, "g")
				_ = os.Remove(s2.path("ghost2"))
				if err := s2.Delete("ghost2"); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s: Delete(ghost2) got %v, want ErrNotExist", ctx, err)
				}
				if idx, _ := s2.readIndex(); idx.Sessions["ghost2"] != nil {
					t.Errorf("%s: Delete(ghost2) 后索引仍有条目 (want removed)", ctx)
				}
			},
		},
	},
	{
		name: "文件不在索引里", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			plantAt(t, s, "b", 1, "b1")
			idx, _ := s.readIndex()
			delete(idx.Sessions, "a")
			if err := s.writeIndex(idx); err != nil {
				t.Fatal(err)
			}
		},
		list: []string{"a", "b"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantOK, pruned: 0,
		extra: map[string]func(*testing.T, *Store, string, error){
			"list": func(t *testing.T, s *Store, ctx string, _ error) {
				if idx, _ := s.readIndex(); idx.Sessions["a"] == nil {
					t.Errorf("%s: List 后索引仍缺 a (want re-indexed)", ctx)
				}
			},
		},
	},
	{
		name: "损坏的索引", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			plantAt(t, s, "b", 1, "b1")
			writeRawSession(t, s.dir, indexFileName, "{not json")
		},
		list: []string{"a", "b"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantOK, pruned: 0,
		note: "索引只是缓存：从文件重建",
	},
	{
		name: "残留的归档目录", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			for _, key := range []string{"ghost", "a"} {
				if err := os.MkdirAll(s.archiveDir(key), 0o700); err != nil {
					t.Fatal(err)
				}
				writeRawSession(t, s.archiveDir(key), "1.jsonl", "old "+key)
			}
		},
		list: []string{"a"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantOK, pruned: 0,
		note: "归档对 List 不可见；AutoPrune 清掉没有会话文件的归档，保留受保护会话的",
		extra: map[string]func(*testing.T, *Store, string, error){
			"prune": func(t *testing.T, s *Store, ctx string, _ error) {
				if !fileExists(s.archiveDir("ghost")) {
					t.Errorf("%s: Prune 删了孤儿归档 (want only AutoPrune to)", ctx)
				}
			},
			"autoprune": func(t *testing.T, s *Store, ctx string, _ error) {
				if fileExists(s.archiveDir("ghost")) || !fileExists(s.archiveDir("a")) {
					t.Errorf("%s: got archive/ghost=%v archive/a=%v, want ghost removed, a kept", ctx, fileExists(s.archiveDir("ghost")), fileExists(s.archiveDir("a")))
				}
			},
			"delete": func(t *testing.T, s *Store, ctx string, _ error) {
				if fileExists(s.archiveDir("a")) {
					t.Errorf("%s: Delete 后 archive/a 仍在 (want removed with the session)", ctx)
				}
			},
		},
	},
	{
		name: "只读文件", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			plantAt(t, s, "b", 1, "b1")
			if err := os.Chmod(s.path("a"), 0o400); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(s.path("a"), 0o600) })
		},
		list: []string{"a", "b"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantErr, delA: wantOK, pruned: 0,
		note: "追加打不开只读文件：Save 必须报错且原文件不变；Delete 照常（Windows 的 os.Remove 会清只读位）",
		extra: map[string]func(*testing.T, *Store, string, error){
			"save": func(t *testing.T, s *Store, ctx string, _ error) {
				r, err := NewStoreAt(s.dir).Load("a")
				if err != nil || len(r.Messages) != 1 || r.Messages[0].Content != "a1" {
					t.Errorf("%s: 失败的 Save 后 a got (%+v, %v), want unchanged [a1]", ctx, r, err)
				}
			},
		},
	},
	{
		name: "文件被另一句柄打开", onlyOS: "windows", setup: func(t *testing.T, s *Store) {
			plantAt(t, s, "a", 0, "a1")
			plantAt(t, s, "b", 1, "b1")
			f, err := os.Open(s.path("a"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
		},
		list: []string{"a", "b"}, loadA: wantOK, loadMsgs: []string{"a1"},
		saveA: wantOK, delA: wantErr, pruned: 0,
		note: "os.Open 不带 FILE_SHARE_DELETE：追加可以共享，删除报共享冲突；Delete 失败时文件与索引条目都保留",
		extra: map[string]func(*testing.T, *Store, string, error){
			"delete": func(t *testing.T, s *Store, ctx string, err error) {
				if errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s: got %v, want a sharing error, not ErrNotExist", ctx, err)
				}
				if !fileExists(s.path("a")) {
					t.Errorf("%s: 删除失败却没有 a.jsonl 了", ctx)
				}
				if idx, _ := s.readIndex(); idx.Sessions["a"] == nil {
					t.Errorf("%s: 删除失败却丢了索引条目 a", ctx)
				}
			},
		},
	},
}

func TestTableSessionDisk(t *testing.T) {
	t.Parallel()
	ops := []string{"list", "save", "delete", "prune", "autoprune"}
	for _, st := range diskStates {
		for _, op := range ops {
			t.Run(st.name+"/"+op, func(t *testing.T) {
				t.Parallel()
				if st.onlyOS == "windows" && !isWindows() {
					t.Skip("Windows 共享冲突在其他系统不可复现")
				}
				if why := st.skip[op]; why != "" {
					skipBug(t, why)
				}
				s, _ := newTableStore(t)
				st.setup(t, s)
				s = NewStoreAt(s.dir) // a fresh process: nothing persisted
				ctx := fmt.Sprintf("状态=%s 出口=%s", st.name, op)
				err := runDiskOp(t, s, st, op, ctx)
				if f := st.extra[op]; f != nil {
					f(t, s, ctx, err)
				}
				checkNoTempResidue(t, s.dir, ctx)
				checkDirLayout(t, s.dir, ctx)
				checkListMatchesIndex(t, s, ctx)
			})
		}
	}
}

func runDiskOp(t *testing.T, s *Store, st diskState, op, ctx string) error {
	t.Helper()
	switch op {
	case "list":
		recs, err := s.List()
		if err != nil {
			t.Fatalf("%s: got %v, want nil", ctx, err)
		}
		var got []string
		for _, r := range recs {
			got = append(got, r.ID)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, st.list) {
			t.Errorf("%s: List IDs got %q, want %q (%s)", ctx, got, st.list, st.note)
		}
		for i := 1; i < len(recs); i++ {
			if recs[i].UpdatedAt.After(recs[i-1].UpdatedAt) {
				t.Errorf("%s: List 未按 UpdatedAt 降序: %q after %q", ctx, recs[i].ID, recs[i-1].ID)
			}
		}
		r, lerr := s.Load("a")
		if !st.loadA.match(lerr) {
			t.Errorf("%s: Load(a) got %v, want %s (%s)", ctx, lerr, st.loadA, st.note)
		}
		if lerr == nil {
			if got := contents(r); !reflect.DeepEqual(got, st.loadMsgs) {
				t.Errorf("%s: Load(a) messages got %q, want %q (%s)", ctx, got, st.loadMsgs, st.note)
			}
		}
		return err

	case "save":
		r, lerr := s.Load("a")
		if lerr != nil {
			r = tableRecord("a")
		}
		base := contents(r)
		r.Messages = append(r.Messages, tableRecord("a", "a-new").Messages...)
		err := s.Save(r)
		if !st.saveA.match(err) {
			t.Errorf("%s: got %v, want %s (%s)", ctx, err, st.saveA, st.note)
		}
		if err == nil {
			got, gerr := NewStoreAt(s.dir).Load("a")
			if want := append(base, "a-new"); gerr != nil || !reflect.DeepEqual(contents(got), want) {
				t.Errorf("%s: 保存后 Load(a) got (%q, %v), want %q", ctx, contents(got), gerr, want)
			}
		}
		return err

	case "delete":
		err := s.Delete("a")
		if !st.delA.match(err) {
			t.Errorf("%s: got %v, want %s (%s)", ctx, err, st.delA, st.note)
		}
		if err == nil && (fileExists(s.path("a")) || fileExists(s.legacyPath("a"))) {
			t.Errorf("%s: Delete 成功但 a.jsonl=%v a.json=%v 仍在", ctx, fileExists(s.path("a")), fileExists(s.legacyPath("a")))
		}
		return err

	case "prune", "autoprune":
		had := fileExists(s.path("a")) || fileExists(s.legacyPath("a"))
		var n int
		var err error
		if op == "prune" {
			n, err = s.Prune(1, "a")
		} else {
			n, err = s.AutoPrune(1, "a")
		}
		if err != nil || n != st.pruned {
			t.Errorf("%s: got (%d, %v), want (%d, nil) (%s)", ctx, n, err, st.pruned, st.note)
		}
		if had && !fileExists(s.path("a")) && !fileExists(s.legacyPath("a")) {
			t.Errorf("%s: 受保护的 a 被删除", ctx)
		}
		return err
	}
	t.Fatalf("unknown op %q", op)
	return nil
}

func contents(r *Record) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, m := range r.Messages {
		out = append(out, m.Content)
	}
	return out
}
