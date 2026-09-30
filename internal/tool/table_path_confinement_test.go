package tool

// T6 路径限制表格测试（docs/superpowers/specs/2026-09-30-test-design.md §4.7）：
// 同一批路径形态喂给每个文件工具的限制入口——resolvePathInCwd（forWrite
// false/true）、read、write、edit、glob、grep、screenshot 输出、draw_image
// 输出——逐条比对“在工作区内/外/ADS”，并检查不变量：
//
//	(a) 同一形态，所有工具的“被限制拒绝”判定一致；
//	(b) forWrite=true 不比 forWrite=false 宽松，且两者解析出同一个路径；
//	(c) Windows 上 ADS 被每个工具拒绝；
//	(d) 目标在外的链接/junction，读写都拒绝；
//	    另外：resolvePathInCwd 接受的路径一定 Within(root)。
//
// Windows 专有形态在 table_path_confinement_windows_test.go 里通过
// platformConfinementForms 追加。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/safepath"
)

// confFixture is the tree every form is judged against:
//
//	base/ws                  the working directory (root)
//	base/ws/a.md a.png sub/{a.md,a.png} longdirectoryname/{a.md,a.png} verylongfilename.md
//	base/ws2/a.md            sibling sharing root's prefix
//	base/out/{a.md,a.png}    outside
//	symlinks (when allowed): sin.md/sin.png -> ws/a.*, sout.md/sout.png -> out/a.*,
//	    sdirin -> ws/sub, sdirout -> out, srelout -> ../out, sdangling.md/.png -> out/missing.*
//	junctions (Windows):     jin -> ws/sub, jout -> out, junctionlongname -> out
type confFixture struct {
	base, root, outside string
	symlinkWhy          string
	junctionWhy         string
	repl                []string // extra template replacements set by the platform file
}

var (
	platformConfinementSetup func(t *testing.T, fx *confFixture)
	platformConfinementForms func(fx *confFixture) []confForm
)

var pngBytes = append(append([]byte{}, pngSignature...), 0, 0, 0, 0)

func newConfFixture(t *testing.T) *confFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := &confFixture{base: base, root: filepath.Join(base, "ws"), outside: filepath.Join(base, "out")}
	for _, f := range []string{
		"ws/a.md", "ws/a.png", "ws/sub/a.md", "ws/sub/a.png",
		"ws/longdirectoryname/a.md", "ws/longdirectoryname/a.png", "ws/verylongfilename.md",
		"ws2/a.md", "out/a.md", "out/a.png",
	} {
		p := filepath.Join(base, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		data := []byte("x\n")
		if strings.HasSuffix(f, ".png") {
			data = pngBytes
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range [][2]string{
		{filepath.Join(fx.root, "a.md"), "sin.md"},
		{filepath.Join(fx.root, "a.png"), "sin.png"},
		{filepath.Join(fx.outside, "a.md"), "sout.md"},
		{filepath.Join(fx.outside, "a.png"), "sout.png"},
		{filepath.Join(fx.root, "sub"), "sdirin"},
		{fx.outside, "sdirout"},
		{filepath.Join("..", "out"), "srelout"},
		{filepath.Join(fx.outside, "missing.md"), "sdangling.md"},
		{filepath.Join(fx.outside, "missing.png"), "sdangling.png"},
	} {
		if err := os.Symlink(l[0], filepath.Join(fx.root, l[1])); err != nil {
			fx.symlinkWhy = fmt.Sprintf("this account cannot create symbolic links (%v)", err)
			break
		}
	}
	fx.junctionWhy = "junctions are a Windows feature"
	if platformConfinementSetup != nil {
		platformConfinementSetup(t, fx)
	}
	return fx
}

// Where a form lies, as every file tool must judge it.
const (
	wantInside  = "inside"  // no tool refuses it for confinement
	wantOutside = "outside" // every tool refuses it
	wantStream  = "stream"  // every tool refuses it as an alternate data stream
	wantAny     = ""        // expectation unsettled (see report): only the invariants
)

// confForm is one row. path is a template: {f} is the file name (a.md for
// the text tools, a.png for the image tools), {e} its extension, {root}
// {out} {base} the fixture directories, {ROOT} root upper-cased; the
// platform file adds more (8.3 names, the volume).
type confForm struct {
	dim    string // 维度值（路径形态）
	path   string
	want   string
	needs  string // "", "symlink", "junction"
	linked bool   // a link is on the path: the lexical check does not apply
	note   string
	skip   string // "bug: ..." for a known product bug
	// noImage, when set, keeps the image tools (screenshot, draw_image) out
	// of this row and says why.
	noImage string
}

func (f confForm) expand(fx *confFixture, name string) string {
	pairs := []string{
		"{f}", name, "{e}", filepath.Ext(name),
		"{root}", fx.root, "{ROOT}", strings.ToUpper(fx.root), "{out}", fx.outside, "{base}", fx.base,
	}
	return strings.NewReplacer(append(pairs, fx.repl...)...).Replace(f.path)
}

// fileForm reports whether the form names a file the image tools can be
// pointed at (a .png); directory-only forms are for the text tools.
func (f confForm) fileForm() bool {
	if f.noImage != "" {
		return false
	}
	return f.path == "" || strings.Contains(f.path, "{f}") || strings.Contains(f.path, "{e}")
}

func portableConfinementForms() []confForm {
	sep := string(filepath.Separator)
	win := runtime.GOOS == "windows"
	mixedWant := wantInside
	if win {
		mixedWant = wantOutside
	}
	return []confForm{
		// ".." 各位置
		{dim: "dotdot-start", path: "../out/{f}", want: wantOutside, note: "开头 .. 爬出工作区"},
		{dim: "dotdot-only", path: "..", want: wantOutside},
		{dim: "dotdot-middle-inside", path: "sub/../{f}", want: wantInside},
		{dim: "dotdot-middle-escape", path: "sub/../../out/{f}", want: wantOutside},
		{dim: "dotdot-end", path: "sub/..", want: wantInside, note: "结尾 .. 回到工作区本身"},
		{dim: "dotdot-end-escape", path: "sub/../..", want: wantOutside},
		{dim: "dotdot-many-escape", path: strings.Repeat("../", 40) + "{f}", want: wantOutside},
		{dim: "dotdot-deep-balanced", path: strings.Repeat("sub/", 30) + strings.Repeat("../", 30) + "{f}", want: wantInside},
		{dim: "dotdot-abs-escape", path: "{root}" + sep + ".." + sep + "out" + sep + "{f}", want: wantOutside},
		{dim: "dotdot-prefix-sibling", path: "../ws2/{f}", want: wantOutside, note: "ws2 以 ws 为前缀"},
		// "." 段
		{dim: "dot-root", path: ".", want: wantInside},
		{dim: "dot-prefix", path: "./{f}", want: wantInside},
		{dim: "dot-middle", path: "sub/./{f}", want: wantInside},
		{dim: "dot-then-dotdot", path: "./../out/{f}", want: wantOutside},
		// 绝对路径
		{dim: "abs-inside", path: "{root}" + sep + "{f}", want: wantInside},
		{dim: "abs-root", path: "{root}", want: wantInside},
		{dim: "abs-outside", path: "{out}" + sep + "{f}", want: wantOutside},
		{dim: "abs-prefix-sibling", path: "{base}" + sep + "ws2" + sep + "{f}", want: wantOutside},
		{dim: "abs-new-file", path: "{root}" + sep + "new" + sep + "dir" + sep + "{f}", want: wantInside},
		// 大小写、尾部点/空格
		{dim: "case-relative", path: "SUB/A{e}", want: wantInside},
		{dim: "trailing-dot", path: "{f}.", want: wantInside, note: "Windows 去掉尾点：仍是工作区内的文件"},
		{dim: "trailing-space", path: "{f} ", want: wantInside},
		{dim: "trailing-dot-dir", path: "sub./{f}", want: wantInside},
		{dim: "dotdot-space", path: ".. /out/{f}", want: wantInside, note: `".. " 不是 ..`},
		// ~、空、NUL
		{dim: "tilde", path: "~/{f}", want: wantInside, note: "~ 不展开"},
		{dim: "empty", path: "", want: wantInside, note: "空路径：read/write/edit/draw 报参数缺失，glob/grep 用 cwd，截图用默认名；都不是越界"},
		{dim: "nul-byte", path: "a\x00{f}", want: wantInside, note: "词法在内；打开时 OS 拒绝 NUL"},
		{dim: "nul-then-escape", path: "a\x00/../../out/{f}", want: wantOutside},
		// 超长
		{dim: "long-component", path: strings.Repeat("n", 300) + "{e}", want: wantInside},
		{dim: "long-total", path: strings.Repeat("d123456789/", 30) + "{f}", want: wantInside},
		{dim: "long-total-escape", path: strings.Repeat("d123456789/", 30) + strings.Repeat("../", 31) + "out/{f}", want: wantOutside},
		// 分隔符
		{dim: "mixed-sep-escape", path: `sub/..\..\out\{f}`, want: mixedWant, note: `Windows 上 \ 是分隔符；其他平台是普通字符`},
		// 符号链接
		{dim: "symlink-file-inside", path: "sin{e}", want: wantInside, needs: "symlink", linked: true},
		{dim: "symlink-file-outside", path: "sout{e}", want: wantOutside, needs: "symlink", linked: true},
		{dim: "symlink-dir-inside", path: "sdirin/{f}", want: wantInside, needs: "symlink", linked: true},
		{dim: "symlink-dir-outside", path: "sdirout/{f}", want: wantOutside, needs: "symlink", linked: true},
		{dim: "symlink-dir-outside-itself", path: "sdirout", want: wantOutside, needs: "symlink", linked: true},
		{dim: "symlink-dir-outside-new", path: "sdirout/new/{f}", want: wantOutside, needs: "symlink", linked: true},
		{dim: "symlink-relative-outside", path: "srelout/{f}", want: wantOutside, needs: "symlink", linked: true},
		{dim: "symlink-dangling-outside", path: "sdangling{e}", want: wantOutside, needs: "symlink", linked: true, note: "悬空链接：写会在外部创建文件"},
	}
}

// confOutcome is what one exit did with one path.
type confOutcome struct {
	refused bool   // refused for confinement (outside / other drive / stream)
	stream  bool   // the refusal names an alternate data stream
	resolve string // resolvePathInCwd's result, for the two resolve exits
	msg     string // the error or result text, for the failure message
	skipped string // why the exit was not run
}

var confinementMarkers = []string{
	"path outside working directory",
	"path on different drive",
	"alternate data stream",
	"must be inside the workspace",
}

func classify(isErr bool, msg string) confOutcome {
	o := confOutcome{msg: msg}
	if !isErr {
		return o
	}
	for _, m := range confinementMarkers {
		if strings.Contains(msg, m) {
			o.refused = true
		}
	}
	o.stream = strings.Contains(msg, "alternate data stream")
	return o
}

func short(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// lexicallyUnder reports whether p is root or below it, as text.
func lexicallyUnder(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	if runtime.GOOS == "windows" {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// runConfinementExits feeds one form to every exit. Mutating tools (write,
// edit, draw_image) only run when resolvePathInCwd refused the path or
// resolved it below root, so a confinement bug cannot write outside the
// test's temp directory (the resolve exits report that bug).
func runConfinementExits(fx *confFixture, f confForm) map[string]confOutcome {
	ctx := context.Background()
	tctx := Context{Cwd: fx.root, PermissionMode: "bypass"}
	out := map[string]confOutcome{}
	text := f.expand(fx, "a.md")

	resolved := map[bool]string{}
	for _, forWrite := range []bool{false, true} {
		p, err := resolvePathInCwd(text, tctx, forWrite)
		var o confOutcome
		if err != nil {
			o = classify(true, err.Error())
		} else {
			o = confOutcome{resolve: p, msg: p}
		}
		resolved[forWrite] = p
		out[fmt.Sprintf("resolvePathInCwd(forWrite=%v)", forWrite)] = o
	}
	safeToMutate := func(p string) bool {
		rp, err := resolvePathInCwd(p, tctx, true)
		return err != nil || (lexicallyUnder(fx.root, rp) && safepath.Within(fx.root, rp))
	}
	call := func(name string, mutates bool, p string, fn func() (Result, error)) {
		if mutates && !safeToMutate(p) {
			out[name] = confOutcome{skipped: "resolvePathInCwd accepted a path that is not below root; not run"}
			return
		}
		res, err := fn()
		if err != nil {
			out[name] = classify(true, err.Error())
			return
		}
		out[name] = classify(res.IsError, res.Data)
	}
	call("read", false, text, func() (Result, error) {
		return NewReadTool().Call(ctx, Input{"filePath": text}, tctx)
	})
	call("glob", false, text, func() (Result, error) {
		return NewGlobTool().Call(ctx, Input{"pattern": "*", "path": text}, tctx)
	})
	call("grep", false, text, func() (Result, error) {
		return NewGrepTool().Call(ctx, Input{"pattern": "never-matches-zq", "path": text}, tctx)
	})
	call("edit", true, text, func() (Result, error) {
		return NewEditTool().Call(ctx, Input{"filePath": text, "oldString": "never-matches-zq", "newString": "y"}, tctx)
	})
	call("write", true, text, func() (Result, error) {
		return NewWriteTool().Call(ctx, Input{"filePath": text, "content": "x\n"}, tctx)
	})
	if f.fileForm() {
		img := f.expand(fx, "a.png")
		if _, err := screenshotPath(Input{"output": img}, fx.root); err != nil {
			out["screenshot"] = classify(true, err.Error())
		} else {
			out["screenshot"] = confOutcome{}
		}
		call("draw_image", true, img, func() (Result, error) {
			return NewDrawImageTool().Call(ctx, Input{
				"outputPath": img, "width": 2.0, "height": 2.0,
				"shapes": []any{map[string]any{"type": "fill"}},
			}, tctx)
		})
	}
	return out
}

func TestTablePathConfinement(t *testing.T) {
	fx := newConfFixture(t)
	forms := portableConfinementForms()
	if platformConfinementForms != nil {
		forms = append(forms, platformConfinementForms(fx)...)
	}
	seen := map[string]bool{}
	for i, f := range forms {
		if seen[f.dim] {
			t.Fatalf("duplicate form %q", f.dim)
		}
		seen[f.dim] = true
		t.Run(fmt.Sprintf("%02d_%s", i, f.dim), func(t *testing.T) {
			if f.skip != "" {
				t.Skip(f.skip)
			}
			switch f.needs {
			case "symlink":
				if fx.symlinkWhy != "" {
					t.Skip("symlink form: " + fx.symlinkWhy)
				}
			case "junction":
				if fx.junctionWhy != "" {
					t.Skip("junction form: " + fx.junctionWhy)
				}
			}
			text := f.expand(fx, "a.md")
			outs := runConfinementExits(fx, f)

			// Expectation per exit.
			var refusedBy, allowedBy []string
			for name, o := range outs {
				if o.skipped != "" {
					t.Errorf("出口 %s [形态=%s] path %q: %s", name, f.dim, text, o.skipped)
					continue
				}
				if o.refused {
					refusedBy = append(refusedBy, name)
				} else {
					allowedBy = append(allowedBy, name)
				}
				switch f.want {
				case wantInside:
					if o.refused {
						t.Errorf("出口 %s [形态=%s] path %q: got refused (%s), want inside (%s)", name, f.dim, text, short(o.msg), f.note)
					}
				case wantOutside:
					if !o.refused {
						t.Errorf("出口 %s [形态=%s] path %q: got allowed (%s), want refused as outside (%s)", name, f.dim, text, short(o.msg), f.note)
					}
				case wantStream:
					if !o.refused || !o.stream {
						t.Errorf("出口 %s [形态=%s] path %q: got %s, want refused as an alternate data stream (%s)", name, f.dim, text, short(o.msg), f.note)
					}
				}
			}
			if f.want == wantAny {
				t.Logf("待确认 [形态=%s] path %q: refused by %v, allowed by %v", f.dim, text, refusedBy, allowedBy)
			}
			// (a) every exit agrees.
			if len(refusedBy) > 0 && len(allowedBy) > 0 {
				t.Errorf("不变量(a) 工具判定一致 [形态=%s] path %q: refused by %v, allowed by %v", f.dim, text, refusedBy, allowedBy)
				for name, o := range outs {
					t.Logf("  %s: refused=%v %s", name, o.refused, short(o.msg))
				}
			}
			// (b) forWrite=true is never more permissive, and resolves the same.
			r, w := outs["resolvePathInCwd(forWrite=false)"], outs["resolvePathInCwd(forWrite=true)"]
			if r.refused && !w.refused {
				t.Errorf("不变量(b) forWrite 不更宽松 [形态=%s] path %q: read refused (%s) but write allowed", f.dim, text, short(r.msg))
			}
			if !r.refused && !w.refused && r.resolve != w.resolve {
				t.Errorf("不变量(b) 同一目标 [形态=%s] path %q: forWrite=false -> %q, forWrite=true -> %q", f.dim, text, r.resolve, w.resolve)
			}
			// An accepted path is inside root, and (without links) below it as text.
			for _, o := range []confOutcome{r, w} {
				if o.refused || o.resolve == "" {
					continue
				}
				if !safepath.Within(fx.root, o.resolve) {
					t.Errorf("不变量 接受即在内 [形态=%s] path %q: resolvePathInCwd accepted %q, not Within(root)", f.dim, text, o.resolve)
				}
				if !f.linked && !lexicallyUnder(fx.root, o.resolve) {
					t.Errorf("不变量 接受即在内 [形态=%s] path %q: resolvePathInCwd accepted %q, not below root", f.dim, text, o.resolve)
				}
			}
		})
	}
	// Nothing leaked into the outside directory through a mutating tool.
	for _, n := range []string{"a.md", "a.png"} {
		data, err := os.ReadFile(filepath.Join(fx.outside, n))
		if err != nil || (n == "a.md" && string(data) != "x\n") || (n == "a.png" && string(data) != string(pngBytes)) {
			t.Errorf("outside file %s changed: %q %v", n, data, err)
		}
	}
	entries, _ := os.ReadDir(fx.outside)
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("outside directory gained entries: %v", names)
	}
}

// Without a working directory resolvePathInCwd confines nothing; forWrite
// still does not change the answer.
func TestTablePathConfinementNoCwd(t *testing.T) {
	for _, p := range []string{"a.md", "../x.md", string(filepath.Separator)} {
		r, rerr := resolvePathInCwd(p, Context{}, false)
		w, werr := resolvePathInCwd(p, Context{}, true)
		if (rerr == nil) != (werr == nil) || r != w {
			t.Errorf("出口 resolvePathInCwd(%q) [形态=no-cwd]: forWrite=false (%q, %v) vs true (%q, %v)", p, r, rerr, w, werr)
		}
	}
}
