package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// T2 会话存储：ID 形态 × 操作（path / Load / Delete / Save / 索引条目 / Prune）。
//
// Every case runs on a fresh store with sentinels planted (plantSentinels):
// files in the parent of s.dir, a sibling session, its archive and its index
// entry. They are verified byte for byte after every operation, then
// checkStoreInvariants runs.

type idCase struct {
	name string
	id   string
	// key is the expected fileKey(id) on Windows; posixKey, when non-nil, on
	// other systems (a backslash or a drive letter is a plain character
	// there). "" means "not a plain name: no such session".
	key      string
	posixKey *string
	// Outcomes; nil derives them from the key: a plain key saves, loads and
	// deletes fine; an empty key is "no such session" for Load and Delete,
	// and Save must refuse it.
	save, load, del         *outcome // lifecycle: Save, then Load, then Delete
	loadMissing, delMissing *outcome // on a store that never saw the ID
	noPrune                 bool     // the ID cannot be saved, so there is nothing to prune
	// skip / skipWindows map a subtest ("missing", "lifecycle", "prune") to
	// a "bug: ..." reason, on every system / only on Windows.
	skip, skipWindows map[string]string
	note              string
}

func ptr[T any](v T) *T { return &v }

func (c idCase) wantKey() string {
	if !isWindows() && c.posixKey != nil {
		return *c.posixKey
	}
	return c.key
}

func (c idCase) out(o *outcome, valid, invalid outcome) outcome {
	if o != nil {
		return *o
	}
	if c.wantKey() == "" {
		return invalid
	}
	return valid
}

func (c idCase) skipFor(t *testing.T, sub string) {
	t.Helper()
	if runKnownBugs() {
		return
	}
	if why := c.skip[sub]; why != "" {
		t.Skip(why)
	}
	if why := c.skipWindows[sub]; why != "" && isWindows() {
		t.Skip(why)
	}
}

var idCases = []idCase{
	{name: "正常uuid", id: "3f2c9a1e-8b7d-4c2e-9f10-2a6b5c4d3e21", key: "3f2c9a1e-8b7d-4c2e-9f10-2a6b5c4d3e21", note: "基线"},
	{name: "上级目录前缀", id: "../evil", key: "evil", note: "Base 把穿越 ID 留在 s.dir 内（store_disk_test 已记录的约定）"},
	{name: "中间含点点", id: "a/../evil", key: "evil", note: "Base 取最后一段"},
	{name: "结尾点点", id: "evil/..", key: "", note: "Base 为 .. → 空 key；曾经 Delete 会 RemoveAll s.dir"},
	{name: "只有点点", id: "..", key: "", note: "Delete(..) 曾删除整个会话目录"},
	{name: "点点斜杠点点", id: "../..", key: "", note: "同上"},
	{name: "归档点点", id: "archive/..", key: "", note: "archiveDir 拼回 s.dir"},
	{name: "三个点", id: "...", key: "...", note: "不是 . 或 ..，是普通名字"},
	{name: "点点开头", id: "..evil", key: "..evil", note: "含 .. 但不是路径段"},
	{name: "点点结尾", id: "evil..", key: "evil..", note: "Windows 只吃整个文件名末尾的点，这里后面还有 .jsonl"},
	{name: "单斜杠", id: "/", key: "", note: "Base(/) 曾指向整个 archive"},
	{name: "斜杠开头", id: "/evil", key: "evil"},
	{name: "斜杠结尾", id: "evil/", key: "evil", note: "Base 去掉结尾分隔符"},
	{name: "单反斜杠", id: `\`, key: "", note: "Windows 分隔符"},
	{name: "反斜杠穿越", id: `..\evil`, key: "evil", posixKey: ptr(""), note: "Windows 上是分隔符；其他系统含 \\ 被拒"},
	{name: "盘符", id: "C:", key: "", posixKey: ptr("C:"), note: "Windows 上只有卷名"},
	{name: "盘符绝对路径", id: `C:\evil`, key: "evil", posixKey: ptr("")},
	{name: "盘符相对路径", id: "C:evil", key: "evil", posixKey: ptr("C:evil")},
	{name: "UNC路径", id: `\\server\share\evil`, key: "evil", posixKey: ptr("")},
	{name: "空串", id: "", key: "", note: "Base(\"\") 为 ."},
	{name: "空白", id: "   ", key: ""},
	{name: "制表符", id: "\t", key: ""},
	{name: "单点", id: ".", key: ""},
	{
		name: "首尾空格", id: " padded ", key: " padded ",
		note: "空格在 .jsonl 之前，Windows 不会吃掉；Store 不修剪 ID（曾经 Prune 经 SessionIDFromFile 修剪成 \"padded \"，删错文件、保护失效）",
	},
	{name: "archive同名", id: "archive", key: "archive", note: "Delete(archive) 只能删 archive/archive，不能删整个归档"},
	{name: "兄弟文件名", id: sentinelSibling + ".jsonl", key: sentinelSibling + ".jsonl", note: "Store 不剥扩展名：映射到 sibling.jsonl.jsonl，不能碰 sibling.jsonl"},
	{name: "长200", id: strings.Repeat("a", 200), key: strings.Repeat("a", 200), note: "206 字节文件名仍在 255 以内；临时文件名由 tempPattern 截短"},
	{
		name: "长300", id: strings.Repeat("b", 300), key: strings.Repeat("b", 300),
		save: ptr(wantErr), load: ptr(wantErr), del: ptr(wantErr),
		loadMissing: ptr(wantErr), delMissing: ptr(wantErr), noPrune: true,
		note: "文件名超 255：Save 必须报错且不留临时文件；Load/Delete 报错即可（Windows 为 ERROR_INVALID_NAME，不是 ErrNotExist）",
	},
	{name: "Unicode", id: "会话-日本語-🙂-ñ", key: "会话-日本語-🙂-ñ"},
	{name: "设备名", id: "CON", key: "CON", note: "Windows 11 上 CON.jsonl 是普通文件；旧系统会指向控制台"},
	{
		name: "冒号流", id: "ab:c", key: "", posixKey: ptr("ab:c"),
		note: "NTFS 上 ab:c.jsonl 是文件 ab 的备用数据流：Windows 上不是普通名字，在创建任何文件前拒绝（曾经留下空的 .cove-tmp-ab）",
	},
	{
		name: "兄弟文件的流", id: sentinelSibling + ".jsonl:x", key: "", posixKey: ptr(sentinelSibling + ".jsonl:x"),
		note: "NTFS 上指向 sibling.jsonl 的数据流：不能碰 sibling.jsonl 的内容",
	},
	{
		name: "NUL字节", id: "a\x00b", key: "a\x00b",
		save: ptr(wantErr), load: ptr(wantErr), del: ptr(wantErr),
		loadMissing: ptr(wantErr), delMissing: ptr(wantErr), noPrune: true,
		note: "系统调用拒绝 NUL：只要报错、不碰别的文件",
	},
	{
		name: "保留ID", id: "index", key: "index",
		save: ptr(wantReserved), load: ptr(wantReserved), del: ptr(wantRefused),
		loadMissing: ptr(wantReserved), delMissing: ptr(wantRefused), noPrune: true,
		note: "index.json 是索引，不是会话（files.go reservedID：No session may use it）",
	},
	{
		name: "保留ID大写", id: "INDEX", key: "INDEX",
		save: ptr(wantReserved), load: ptr(wantReserved), del: ptr(wantRefused),
		loadMissing: ptr(wantReserved), delMissing: ptr(wantRefused), noPrune: true,
		note: "isReservedID 忽略大小写（Windows 上 INDEX.json 就是 index.json）",
	},
}

func TestTableSessionIDs(t *testing.T) {
	t.Parallel()
	for _, c := range idCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}

// run is one ID case: the pure path checks, then Load/Delete of the unseen
// ID, then Save-Load-Delete, then Prune, all on one store with one set of
// sentinels (planting them costs more than the operations on Windows).
func (c idCase) run(t *testing.T) {
	ctx := func(op string) string {
		return fmt.Sprintf("id=%q key=%q 出口=%s", c.id, c.wantKey(), op)
	}
	s, root := newTableStore(t)
	sn := plantSentinels(t, s, root)
	t.Run("path", func(t *testing.T) {
		key := fileKey(c.id)
		if key != c.wantKey() {
			t.Fatalf("%s: got %q, want %q (%s)", ctx("fileKey"), key, c.wantKey(), c.note)
		}
		if key == "" {
			return
		}
		for op, p := range map[string]string{"path": s.path(c.id), "legacyPath": s.legacyPath(c.id)} {
			if filepath.Dir(p) != s.dir {
				t.Errorf("%s: dir got %q, want %q", ctx(op), filepath.Dir(p), s.dir)
			}
		}
		if got, want := filepath.Base(s.path(c.id)), key+jsonlExt; got != want {
			t.Errorf("%s: base got %q, want %q", ctx("path"), got, want)
		}
		if got, want := filepath.Base(s.legacyPath(c.id)), key+legacyExt; got != want {
			t.Errorf("%s: base got %q, want %q", ctx("legacyPath"), got, want)
		}
		if got, want := filepath.Dir(s.archiveDir(key)), filepath.Join(s.dir, "archive"); got != want {
			t.Errorf("%s: parent got %q, want %q", ctx("archiveDir"), got, want)
		}
	})

	// Load and Delete of an ID the store has never seen.
	t.Run("missing", func(t *testing.T) {
		c.skipFor(t, "missing")

		want := c.out(c.loadMissing, wantNotExist, wantNotExist)
		r, err := s.Load(c.id)
		if !want.match(err) {
			t.Errorf("%s: got (%v, %v), want %s (%s)", ctx("Load"), r, err, want, c.note)
		}
		sn.verify(t, s, ctx("Load"))
		checkStoreInvariants(t, s, ctx("Load"))

		want = c.out(c.delMissing, wantNotExist, wantNotExist)
		err = s.Delete(c.id)
		if !want.match(err) {
			t.Errorf("%s: got %v, want %s (%s)", ctx("Delete"), err, want, c.note)
		}
		sn.verify(t, s, ctx("Delete"))
		checkStoreInvariants(t, s, ctx("Delete"))
	})

	t.Run("lifecycle", func(t *testing.T) {
		c.skipFor(t, "lifecycle")
		key := c.wantKey()
		content := "hello " + c.name

		wantSave := c.out(c.save, wantOK, wantErr)
		err := s.Save(tableRecord(c.id, content))
		if !wantSave.match(err) {
			t.Fatalf("%s: got %v, want %s (%s)", ctx("Save"), err, wantSave, c.note)
		}
		if key == "" && !errors.Is(err, ErrInvalidID) {
			t.Errorf("%s: got %v, want ErrInvalidID (%s)", ctx("Save"), err, c.note)
		}
		sn.verify(t, s, ctx("Save"))
		checkStoreInvariants(t, s, ctx("Save"))
		saved := err == nil
		if saved {
			idx, ierr := s.readIndex()
			if ierr != nil {
				t.Fatalf("%s: readIndex: %v", ctx("index"), ierr)
			}
			if e := idx.Sessions[key]; e == nil || e.ID != c.id || e.File != key+jsonlExt {
				t.Errorf("%s: entry got %+v, want ID=%q File=%q", ctx("index"), e, c.id, key+jsonlExt)
			}
			recs, _ := s.List()
			listed := false
			for _, r := range recs {
				listed = listed || r.ID == c.id
			}
			if !listed {
				t.Errorf("%s: got %d records without %q, want it listed", ctx("List"), len(recs), c.id)
			}
		}

		wantLoad := c.out(c.load, wantOK, wantNotExist)
		r, err := s.Load(c.id)
		if !wantLoad.match(err) {
			t.Errorf("%s: got %v, want %s (%s)", ctx("Load"), err, wantLoad, c.note)
		}
		if err == nil && (r.ID != c.id || len(r.Messages) != 1 || r.Messages[0].Content != content) {
			t.Errorf("%s: record got ID=%q msgs=%+v, want ID=%q content=%q", ctx("Load"), r.ID, r.Messages, c.id, content)
		}
		sn.verify(t, s, ctx("Load"))
		checkStoreInvariants(t, s, ctx("Load"))

		wantDel := c.out(c.del, wantOK, wantNotExist)
		err = s.Delete(c.id)
		if !wantDel.match(err) {
			t.Errorf("%s: got %v, want %s (%s)", ctx("Delete"), err, wantDel, c.note)
		}
		sn.verify(t, s, ctx("Delete"))
		checkStoreInvariants(t, s, ctx("Delete"))
		if saved && err == nil {
			if _, serr := os.Stat(s.path(c.id)); !errors.Is(serr, fs.ErrNotExist) {
				t.Errorf("%s: 文件仍在: stat got %v, want ErrNotExist", ctx("Delete"), serr)
			}
			if idx, _ := s.readIndex(); idx.Sessions[key] != nil {
				t.Errorf("%s: index 仍有条目 %q", ctx("Delete"), key)
			}
			if _, lerr := s.Load(c.id); !errors.Is(lerr, fs.ErrNotExist) {
				t.Errorf("%s: Load after Delete got %v, want ErrNotExist", ctx("Load"), lerr)
			}
		}
	})

	// Prune with the ID older than the sibling: protected it survives;
	// unprotected it is the one removed, and only its own file goes.
	t.Run("prune", func(t *testing.T) {
		if c.wantKey() == "" || c.noPrune {
			t.Skip("ID 不能保存，无可清理")
		}
		c.skipFor(t, "prune")
		key := c.wantKey()
		if err := s.Save(tableRecord(c.id, "old")); err != nil {
			t.Fatalf("%s: %v", ctx("Save"), err)
		}
		s.setIndexUpdatedAt(t, key, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))

		n, err := s.Prune(1, c.id)
		if err != nil || n != 0 {
			t.Errorf("%s: protect=%q got (%d, %v), want (0, nil)", ctx("Prune"), c.id, n, err)
		}
		if _, serr := os.Stat(s.path(c.id)); serr != nil {
			t.Errorf("%s: 受保护的 %q 被删: %v", ctx("Prune"), c.id, serr)
		}
		sn.verify(t, s, ctx("Prune(protect)"))
		checkStoreInvariants(t, s, ctx("Prune(protect)"))

		n, err = s.Prune(1)
		if err != nil || n != 1 {
			t.Errorf("%s: got (%d, %v), want (1, nil)", ctx("Prune"), n, err)
		}
		if _, serr := os.Stat(s.path(c.id)); !errors.Is(serr, fs.ErrNotExist) {
			t.Errorf("%s: 最旧的 %q 应被删除: stat got %v, want ErrNotExist", ctx("Prune"), c.id, serr)
		}
		sn.verify(t, s, ctx("Prune"))
		checkStoreInvariants(t, s, ctx("Prune"))
	})
}

// Two IDs that differ only by case name one file on a case-insensitive file
// system. Saving the second then replaces the first one's transcript; the
// archive-before-rewrite design (changedBehind) must keep it.
func TestTableSessionIDsCaseCollision(t *testing.T) {
	t.Parallel()
	s, _ := newTableStore(t)
	if err := s.Save(tableRecord("CaseA", "upper turn")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(tableRecord("casea", "lower turn")); err != nil {
		t.Fatal(err)
	}
	ctx := fmt.Sprintf("ids=%q/%q 出口=Save", "CaseA", "casea")
	checkStoreInvariants(t, s, ctx)
	if !caseInsensitiveFS(t, s.dir) {
		for id, want := range map[string]string{"CaseA": "upper turn", "casea": "lower turn"} {
			r, err := s.Load(id)
			if err != nil || len(r.Messages) != 1 || r.Messages[0].Content != want {
				t.Errorf("%s: Load(%q) got (%+v, %v), want content %q", ctx, id, r, err, want)
			}
		}
		return
	}
	live, _ := os.ReadFile(s.path("casea"))
	kept := strings.Contains(string(live), "upper turn") ||
		archivedContaining(t, s, "CaseA", "upper turn") || archivedContaining(t, s, "casea", "upper turn")
	if !kept {
		t.Errorf("%s: 大小写不敏感文件系统上 CaseA 的轮次既不在现文件也不在归档 (got lost, want archived)", ctx)
	}
	if !strings.Contains(string(live), "lower turn") {
		t.Errorf("%s: 现文件 got %q, want it to hold %q", ctx, live, "lower turn")
	}
}

// onWindows picks an expected outcome by system.
func onWindows(win, other outcome) *outcome {
	if isWindows() {
		return &win
	}
	return &other
}
