package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// T2 会话存储：并发。Two Store instances stand for two cove processes on one
// sessions directory. Run under -race; every wait is bounded.

// liveOrArchived reports whether text is in id's live file or in one of its
// archived transcripts.
func liveOrArchived(t *testing.T, s *Store, id, text string) bool {
	t.Helper()
	if data, err := os.ReadFile(s.path(id)); err == nil && strings.Contains(string(data), text) {
		return true
	}
	return archivedContaining(t, s, fileKey(id), text)
}

// A step is one writer ("A" or "B") saving the next turn of its own history
// of the shared session; "B<" first loads the file (as /resume does), "B" keeps
// whatever history it had. Every turn a Save accepted must survive in the
// live file or the archive (archive-before-rewrite, see Store.save).
type alternateCase struct {
	name  string
	steps []string
	note  string
}

var alternateCases = []alternateCase{
	{name: "B先加载后交替", steps: []string{"A", "B<", "A", "B", "A"}, note: "concurrent_writer_test 的延伸：每次对方改过文件都先归档"},
	{name: "B未加载直接写", steps: []string{"A", "B", "A", "B"}, note: "B 从未读过文件：changedBehind 把 A 的版本归档"},
	{name: "一方连写", steps: []string{"A", "A", "B<", "B", "B", "A"}, note: "连写走追加；A 最后一次必须发现 B 的改动"},
	{name: "双方都先加载", steps: []string{"A", "B<", "A<", "B", "A"}, note: "A< 重新加载后合并到 B 的版本"},
}

func TestTableSessionConcurrencyAlternate(t *testing.T) {
	t.Parallel()
	for _, c := range alternateCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newTableStore(t)
			stores := map[byte]*Store{'A': a, 'B': NewStoreAt(a.dir)}
			recs := map[byte]*Record{'A': tableRecord("shared"), 'B': tableRecord("shared")}
			var accepted []string
			for i, step := range c.steps {
				w := step[0]
				s := stores[w]
				ctx := fmt.Sprintf("steps=%q 第%d步=%q 出口=Save", c.steps, i, step)
				if strings.HasSuffix(step, "<") {
					r, err := s.Load("shared")
					if err != nil {
						t.Fatalf("%s: Load got %v, want ok", ctx, err)
					}
					recs[w] = r
				}
				turn := fmt.Sprintf("turn %d from %c", i, w)
				recs[w].Messages = append(recs[w].Messages, tableRecord("", turn).Messages...)
				if err := s.Save(recs[w]); err != nil {
					t.Fatalf("%s: got %v, want nil", ctx, err)
				}
				accepted = append(accepted, turn)
				for _, text := range accepted {
					if !liveOrArchived(t, a, "shared", text) {
						t.Errorf("%s: 已保存的 %q 既不在现文件也不在归档 (got lost, want kept) (%s)", ctx, text, c.note)
					}
				}
				checkStoreInvariants(t, s, ctx)
			}
			// The live file is the last writer's history, whole.
			last := c.steps[len(c.steps)-1][0]
			got, err := NewStoreAt(a.dir).Load("shared")
			if err != nil || !reflect.DeepEqual(contents(got), contents(recs[last])) {
				t.Errorf("steps=%q 出口=Load: got (%q, %v), want the last writer's %q", c.steps, contents(got), err, contents(recs[last]))
			}
		})
	}
}

// startTogether runs fns on their own goroutines, released at once, and
// waits for all of them, failing the test if that takes longer than limit.
func startTogether(t *testing.T, limit time.Duration, fns ...func()) {
	t.Helper()
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			fn()
		}()
	}
	close(gate)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("并发操作 %v 内未结束", limit)
	}
}

// Two stores saving one session truly at once. Nothing about the order is
// fixed, so only what must hold whatever the interleaving is checked: no
// panic or race, no temp residue, the file still loads cleanly, and the
// final save of each writer (run after both loops) is intact.
func TestTableSessionConcurrencySimultaneousSaves(t *testing.T) {
	t.Parallel()
	a, _ := newTableStore(t)
	b := NewStoreAt(a.dir)
	const n = 4
	var errsMu sync.Mutex
	var errs []error
	writer := func(s *Store, name string) func() {
		return func() {
			r := tableRecord("shared")
			for i := 0; i < n; i++ {
				r.Messages = append(r.Messages, tableRecord("", fmt.Sprintf("%s-%d", name, i)).Messages...)
				if err := s.Save(r); err != nil {
					errsMu.Lock()
					errs = append(errs, err)
					errsMu.Unlock()
				}
			}
		}
	}
	startTogether(t, 20*time.Second, writer(a, "A"), writer(b, "B"))
	for _, err := range errs {
		// A rename can lose a sharing race on Windows after its retries;
		// that is reported, not hidden, so it is allowed here.
		t.Logf("并发 Save 报错（允许）: %v", err)
	}
	ctx := "并发 Save×Save 出口=Load"
	checkNoTempResidue(t, a.dir, ctx)
	data, err := os.ReadFile(a.path("shared"))
	if err != nil {
		t.Fatalf("%s: got %v, want the file", ctx, err)
	}
	if _, clean, perr := parseJSONL(data); perr != nil || !clean {
		t.Errorf("%s: 文件 got clean=%v err=%v, want a clean file:\n%s", ctx, clean, perr, data)
	}
	// Settle: one more save from each, in turn, must keep both histories.
	for _, w := range []struct {
		s    *Store
		name string
	}{{a, "A"}, {b, "B"}} {
		r := tableRecord("shared")
		for i := 0; i < n; i++ {
			r.Messages = append(r.Messages, tableRecord("", fmt.Sprintf("%s-%d", w.name, i)).Messages...)
		}
		r.Messages = append(r.Messages, tableRecord("", w.name+"-final").Messages...)
		if err := w.s.Save(r); err != nil {
			t.Fatalf("%s: settle Save(%s) got %v, want nil", ctx, w.name, err)
		}
	}
	for _, text := range []string{"A-final", "B-final"} {
		if !liveOrArchived(t, a, "shared", text) {
			t.Errorf("%s: %q got lost, want live or archived", ctx, text)
		}
	}
	checkStoreInvariants(t, a, ctx)
}

// Save of the protected session while Prune runs, through one store (the
// lock orders them) and through two (two processes).
func TestTableSessionConcurrencySaveVsPrune(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{true, false} {
		name := map[bool]string{true: "同一Store", false: "两个Store"}[shared]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTableStore(t)
			for i, id := range []string{"o1", "o2", "o3", "o4"} {
				plantAt(t, s, id, i, id)
			}
			plantAt(t, s, "live", -10, "live-0") // oldest: only protect keeps it
			saver := NewStoreAt(s.dir)
			pruner := saver
			if !shared {
				pruner = NewStoreAt(s.dir)
			}
			live, err := saver.Load("live")
			if err != nil {
				t.Fatal(err)
			}
			const saves = 8
			var saveErrs, pruneErrs []error
			var lost []string
			startTogether(t, 20*time.Second,
				func() {
					for i := 1; i <= saves; i++ {
						live.Messages = append(live.Messages, tableRecord("", fmt.Sprintf("live-%d", i)).Messages...)
						if err := saver.Save(live); err != nil {
							saveErrs = append(saveErrs, err)
						}
					}
				},
				func() {
					for i := 0; i < 4; i++ {
						if _, err := pruner.Prune(1, "live"); err != nil {
							pruneErrs = append(pruneErrs, err)
						}
						if !fileExists(pruner.path("live")) {
							lost = append(lost, fmt.Sprintf("after Prune #%d", i))
						}
					}
				})
			ctx := fmt.Sprintf("%s Save×Prune(1, protect=live)", name)
			if len(saveErrs) > 0 {
				t.Errorf("%s 出口=Save: got %v, want nil", ctx, saveErrs)
			}
			for _, err := range pruneErrs {
				t.Logf("%s 出口=Prune: 报错（索引替换可能与另一进程冲突，允许）: %v", ctx, err)
			}
			if len(lost) > 0 {
				t.Errorf("%s 出口=Prune: 受保护的 live 不见了 %v", ctx, lost)
			}
			// A last Prune with nothing racing settles the others.
			if _, err := pruner.Prune(1, "live"); err != nil {
				t.Errorf("%s 出口=Prune: final got %v, want nil", ctx, err)
			}
			got, err := NewStoreAt(s.dir).Load("live")
			var want []string
			for i := 0; i <= saves; i++ {
				want = append(want, fmt.Sprintf("live-%d", i))
			}
			if err != nil || !reflect.DeepEqual(contents(got), want) {
				t.Errorf("%s 出口=Load(live): got (%q, %v), want %q", ctx, contents(got), err, want)
			}
			var others int
			for _, id := range []string{"o1", "o2", "o3", "o4"} {
				if fileExists(s.path(id)) {
					others++
				}
			}
			if others != 1 {
				t.Errorf("%s 出口=Prune: 未受保护的会话 got %d left, want 1 (keep=1)", ctx, others)
			}
			checkStoreInvariants(t, s, ctx)
		})
	}
}

// Delete while other stores Load the same session: every Load sees the
// whole session or ErrNotExist, never a partial or a parse error; the
// sibling is untouched. On Windows a Delete can hit a sharing violation
// while a Load holds the file open; it then reports the error and is retried.
func TestTableSessionConcurrencyDeleteVsLoad(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{true, false} {
		name := map[bool]string{true: "同一Store", false: "多个Store"}[shared]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTableStore(t)
			plantAt(t, s, "x", 0, "x1", "x2", "x3")
			plantAt(t, s, "y", 1, "y1")
			yBefore, _ := os.ReadFile(s.path("y"))
			deleter := NewStoreAt(s.dir)
			stop := make(chan struct{})
			var mu sync.Mutex
			var bad []string
			loader := func() {
				ls := deleter
				if !shared {
					ls = NewStoreAt(s.dir)
				}
				for {
					select {
					case <-stop:
						return
					default:
					}
					r, err := ls.Load("x")
					switch {
					case err == nil && reflect.DeepEqual(contents(r), []string{"x1", "x2", "x3"}):
					case errors.Is(err, fs.ErrNotExist):
						return // gone for good
					case err != nil && r == nil && !strings.Contains(err.Error(), "parse session"):
						// Windows: opening a file whose delete is pending
						// fails with a sharing/access error. An I/O error,
						// not partial data; the loader tries again.
						continue
					default:
						mu.Lock()
						bad = append(bad, fmt.Sprintf("(%q, %v)", contents(r), err))
						mu.Unlock()
						return
					}
				}
			}
			var delErr error
			var retries int
			del := func() {
				defer close(stop)
				deadline := time.Now().Add(5 * time.Second)
				for {
					delErr = deleter.Delete("x")
					if delErr == nil || errors.Is(delErr, fs.ErrNotExist) || time.Now().After(deadline) {
						return
					}
					retries++
					time.Sleep(5 * time.Millisecond)
				}
			}
			startTogether(t, 20*time.Second, loader, loader, loader, del)
			ctx := fmt.Sprintf("%s Delete(x)×Load(x)", name)
			if len(bad) > 0 {
				t.Errorf("%s 出口=Load: got %v, want the whole session or ErrNotExist", ctx, bad)
			}
			if delErr != nil && !errors.Is(delErr, fs.ErrNotExist) {
				t.Errorf("%s 出口=Delete: 重试 %d 次后 got %v, want nil", ctx, retries, delErr)
			}
			if retries > 0 {
				t.Logf("%s: Delete 因共享冲突重试了 %d 次", ctx, retries)
			}
			if fileExists(s.path("x")) {
				t.Errorf("%s 出口=Delete: x.jsonl 仍在", ctx)
			}
			if _, err := deleter.Load("x"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s 出口=Load: after Delete got %v, want ErrNotExist", ctx, err)
			}
			if yAfter, _ := os.ReadFile(s.path("y")); string(yAfter) != string(yBefore) {
				t.Errorf("%s: 其他会话 y 被改写: got %q, want %q", ctx, yAfter, yBefore)
			}
			if _, err := os.Stat(filepath.Join(s.dir, "y.jsonl")); err != nil {
				t.Errorf("%s: y.jsonl 不见了: %v", ctx, err)
			}
			checkStoreInvariants(t, s, ctx)
		})
	}
}
