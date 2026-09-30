package safepath

// T6 路径限制表格测试（docs/superpowers/specs/2026-09-30-test-design.md §4.7）：
// 同一批路径形态喂给 Within、Join、ValidateName，逐条比对期望，并检查
// 不变量：
//
//   - 路径上没有链接时，Within 与“Clean 后按 filepath.Rel 判包含”一致；
//   - 相对 target 与 filepath.Join(root, target) 判定相同；
//   - Join(root, name) 要么报错（且与 ValidateName 同进退），要么结果正好在
//     root 下一层并且 Within(root)。
//
// Windows 专有形态（junction、8.3 短名、ADS、UNC、盘符……）在
// table_safepath_windows_test.go 里通过 platformWithinForms 追加。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withinFixture is the file tree every form is judged against:
//
//	base/ws                      root
//	base/ws/a.md, sub/b.md, longdirectoryname/c.md, verylongfilename.md
//	base/ws2/a.md                sibling sharing root's prefix
//	base/out/secret.md           outside
//	base/ws/sin.md -> ws/a.md, sout.md -> out/secret.md, sdirin -> ws/sub,
//	base/ws/sdirout -> out, srelout -> ../out, sdangling -> out/missing.md
//	                             (symlinks; only when the account may create them)
//	base/ws/jin -> ws/sub, jout -> out, junctionlongname -> out
//	                             (junctions, Windows only)
type withinFixture struct {
	base, root, outside string
	symlinkWhy          string // why symlinks are unavailable; "" = available
	junctionWhy         string // why junctions are unavailable; "" = available
}

// platformWithinSetup and platformWithinForms are set by the Windows file.
var (
	platformWithinSetup func(t *testing.T, fx *withinFixture)
	platformWithinForms func(fx *withinFixture) []withinForm
)

func newWithinFixture(t *testing.T) *withinFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	fx := &withinFixture{base: base, root: filepath.Join(base, "ws"), outside: filepath.Join(base, "out")}
	for _, f := range []string{
		"ws/a.md", "ws/sub/b.md", "ws/longdirectoryname/c.md", "ws/verylongfilename.md",
		"ws2/a.md", "out/secret.md",
	} {
		p := filepath.Join(base, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	links := [][2]string{
		{filepath.Join(fx.root, "a.md"), "sin.md"},
		{filepath.Join(fx.outside, "secret.md"), "sout.md"},
		{filepath.Join(fx.root, "sub"), "sdirin"},
		{fx.outside, "sdirout"},
		{filepath.Join("..", "out"), "srelout"},
		{filepath.Join(fx.outside, "missing.md"), "sdangling"},
	}
	for _, l := range links {
		if err := os.Symlink(l[0], filepath.Join(fx.root, l[1])); err != nil {
			fx.symlinkWhy = fmt.Sprintf("this account cannot create symbolic links (%v)", err)
			break
		}
	}
	fx.junctionWhy = "junctions are a Windows feature"
	if platformWithinSetup != nil {
		platformWithinSetup(t, fx)
	}
	return fx
}

// withinForm is one row: a path form (dimension value) and where it lies.
type withinForm struct {
	dim    string                         // 维度值（路径形态）
	target func(fx *withinFixture) string // the target handed to Within
	want   bool                           // Within(root, target)
	needs  string                         // "", "symlink", "junction"
	linked bool                           // a link is on the path: the lexical invariant does not apply
	nameOK bool                           // ValidateName(target) accepts it
	note   string                         // the bug class this row guards
	skip   string                         // "bug: ..." for a known product bug
	// unsettled rows have no agreed expectation yet (see the report): only
	// the invariants are checked, and the result is logged.
	unsettled bool
}

func rel(p string) func(*withinFixture) string {
	return func(*withinFixture) string { return filepath.FromSlash(p) }
}

func raw(p string) func(*withinFixture) string {
	return func(*withinFixture) string { return p }
}

func underRoot(p string) func(*withinFixture) string {
	return func(fx *withinFixture) string { return filepath.Join(fx.root, filepath.FromSlash(p)) }
}

func portableWithinForms() []withinForm {
	win := runtime.GOOS == "windows"
	sep := string(filepath.Separator)
	return []withinForm{
		// ".." 各位置
		{dim: "dotdot-start", target: rel("../out/secret.md"), want: false, note: "开头 .. 爬出 root"},
		{dim: "dotdot-only", target: rel(".."), want: false, note: "单独 .. 是父目录"},
		{dim: "dotdot-middle-inside", target: rel("sub/../a.md"), want: true, note: "中间 .. 仍在 root 内"},
		{dim: "dotdot-middle-escape", target: rel("sub/../../out/secret.md"), want: false, note: "中间 .. 爬出 root"},
		{dim: "dotdot-end", target: rel("sub/.."), want: true, note: "结尾 .. 回到 root 本身"},
		{dim: "dotdot-end-escape", target: rel("sub/../.."), want: false, note: "结尾 .. 到 root 的父目录"},
		{dim: "dotdot-after-file", target: rel("a.md/.."), want: true, note: "文件名后的 .. 按词法清理"},
		{dim: "dotdot-many-escape", target: rel(strings.Repeat("../", 40) + "x.md"), want: false, note: "大量 .. 越过卷根也不能算在内"},
		{dim: "dotdot-deep-balanced", target: rel(strings.Repeat("sub/", 40) + strings.Repeat("../", 40) + "a.md"), want: true, note: "深层下降再等量爬升仍在内"},
		{dim: "dotdot-deep-plus-one", target: rel(strings.Repeat("sub/", 3) + strings.Repeat("../", 4) + "out/secret.md"), want: false, note: "多爬一层即在外"},
		{dim: "dotdot-abs-escape", target: func(fx *withinFixture) string { return fx.root + sep + ".." + sep + "out" }, want: false, note: "绝对路径里的 .."},
		// "." 段
		{dim: "dot-root", target: rel("."), want: true, note: "root 本身在内"},
		{dim: "dot-prefix", target: rel("./a.md"), want: true},
		{dim: "dot-middle", target: rel("sub/./b.md"), want: true},
		{dim: "dot-then-dotdot", target: rel("./../out"), want: false},
		// 绝对路径与前缀兄弟
		{dim: "abs-inside", target: underRoot("a.md"), want: true},
		{dim: "abs-root", target: func(fx *withinFixture) string { return fx.root }, want: true},
		{dim: "abs-root-trailing-sep", target: func(fx *withinFixture) string { return fx.root + sep }, want: true},
		{dim: "abs-outside", target: func(fx *withinFixture) string { return filepath.Join(fx.outside, "secret.md") }, want: false},
		{dim: "abs-prefix-sibling", target: func(fx *withinFixture) string { return filepath.Join(fx.base, "ws2", "a.md") }, want: false, note: "ws2 以 ws 为前缀，但按整段比较在外"},
		{dim: "abs-new-file", target: underRoot("new/dir/x.md"), want: true, note: "不存在的文件按其目录判定"},
		// 大小写（相对形态在所有平台都在 root 内；绝对形态见 Windows 文件）
		{dim: "case-relative", target: rel("A.MD"), want: true, nameOK: true},
		// 尾部点/空格
		{dim: "trailing-dot", target: rel("a.md."), want: true, note: "Windows 会去掉尾点，仍是 root 内的 a.md"},
		{dim: "trailing-space", target: rel("a.md "), want: true},
		{dim: "trailing-dot-dir", target: rel("sub./b.md"), want: true},
		{dim: "dotdot-space", target: rel(".. /out/secret.md"), want: true, note: `".. " 不是 ..（Windows 也不把它规范成 ..，已实测打开失败）`},
		// ~、空、NUL、URL 形态
		{dim: "tilde", target: rel("~"), want: true, note: "~ 不展开，是 root 下名为 ~ 的项"},
		{dim: "tilde-slash", target: rel("~/.ssh/id_rsa"), want: true},
		{dim: "empty", target: raw(""), want: false, note: "文档：空 target 不在任何目录内"},
		{dim: "nul-byte", target: raw("a\x00.md"), want: true, note: "词法在内；打开时 OS 拒绝 NUL"},
		{dim: "nul-then-escape", target: rel("a\x00/../../out/secret.md"), want: false},
		{dim: "file-url", target: raw("file:///etc/passwd"), want: true, note: "非绝对路径，按相对 root 处理"},
		// 超长
		{dim: "long-component", target: rel(strings.Repeat("n", 300) + ".md"), want: true, note: "> 255 的单段：Lstat 失败按不存在处理"},
		{dim: "long-total", target: rel(strings.Repeat("d123456789/", 30) + "x.md"), want: true, note: "总长 > 260"},
		{dim: "long-total-escape", target: rel(strings.Repeat("d123456789/", 30) + strings.Repeat("../", 31) + "out/secret.md"), want: false},
		// 分隔符：Windows 上 / 与 \ 等价；其他平台 \ 是普通字符
		{dim: "mixed-sep", target: raw(`sub\b.md`), want: true},
		{dim: "mixed-sep-escape", target: raw(`sub/..\..\out\secret.md`), want: !win, note: `Windows: \ 也是分隔符，爬出 root；其他平台 "..\..\out\secret.md" 是一个普通文件名`},
		// 纯名字（ValidateName 应接受）
		{dim: "plain-name", target: rel("a.md"), want: true, nameOK: true},
		{dim: "plain-long-name", target: rel("verylongfilename.md"), want: true, nameOK: true},
		// 符号链接
		{dim: "symlink-file-inside", target: rel("sin.md"), want: true, needs: "symlink", linked: true, nameOK: true},
		{dim: "symlink-file-outside", target: rel("sout.md"), want: false, needs: "symlink", linked: true, nameOK: true, note: "指向外部的文件链接"},
		{dim: "symlink-dir-inside", target: rel("sdirin/b.md"), want: true, needs: "symlink", linked: true},
		{dim: "symlink-dir-outside", target: rel("sdirout/secret.md"), want: false, needs: "symlink", linked: true},
		{dim: "symlink-dir-outside-new", target: rel("sdirout/new/x.md"), want: false, needs: "symlink", linked: true, note: "外链目录下的新文件"},
		{dim: "symlink-dir-outside-itself", target: rel("sdirout"), want: false, needs: "symlink", linked: true, nameOK: true},
		{dim: "symlink-relative-outside", target: rel("srelout/secret.md"), want: false, needs: "symlink", linked: true, note: "相对目标 ../out"},
		{dim: "symlink-dangling-outside", target: rel("sdangling"), want: false, needs: "symlink", linked: true, nameOK: true, note: "悬空链接按它指向的位置判定"},
		{dim: "symlink-abs-outside", target: underRoot("sdirout/secret.md"), want: false, needs: "symlink", linked: true},
	}
}

// lexicalInside is containment by filepath.Rel on the cleaned absolute path,
// case-insensitively on Windows — what Within must agree with when no link
// is on the path.
func lexicalInside(root, target string) bool {
	if root == "" || target == "" {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	root, target = filepath.Clean(root), filepath.Clean(target)
	if runtime.GOOS == "windows" {
		root, target = strings.ToLower(root), strings.ToLower(target)
	}
	r, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

// isExistingLink reports whether p itself is a link (symlink or junction).
func isExistingLink(p string) bool {
	m, err := linkMode(p)
	return err == nil && isLinkMode(m)
}

func TestTablePathSafepath(t *testing.T) {
	fx := newWithinFixture(t)
	forms := portableWithinForms()
	if platformWithinForms != nil {
		forms = append(forms, platformWithinForms(fx)...)
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
			target := f.target(fx)

			got := Within(fx.root, target)
			if f.unsettled {
				t.Logf("待确认 [形态=%s] Within(root, %q) = %v", f.dim, target, got)
			} else if got != f.want {
				t.Errorf("出口 Within(root, %q) [形态=%s]: got %v, want %v (%s)", target, f.dim, got, f.want, f.note)
			}
			if !f.linked {
				if lex := lexicalInside(fx.root, target); got != lex {
					t.Errorf("不变量 Within==Rel 包含 [形态=%s] target %q: Within %v, Rel 判定 %v", f.dim, target, got, lex)
				}
			}
			// A relative target resolves against root, not the process cwd.
			if !filepath.IsAbs(target) && target != "" {
				if abs := Within(fx.root, filepath.Join(fx.root, target)); abs != got {
					t.Errorf("不变量 相对==绝对 [形态=%s] target %q: relative %v, joined %v", f.dim, target, got, abs)
				}
			}

			nameErr := ValidateName("plugin", target)
			if (nameErr == nil) != f.nameOK {
				t.Errorf("出口 ValidateName(%q) [形态=%s]: got err=%v, want accepted=%v", target, f.dim, nameErr, f.nameOK)
			}
			joined, joinErr := Join("plugin", fx.root, target)
			if (joinErr == nil) != (nameErr == nil) {
				t.Errorf("出口 Join(root, %q) [形态=%s]: err %v, but ValidateName err %v", target, f.dim, joinErr, nameErr)
			}
			if joinErr == nil {
				if filepath.Dir(joined) != fx.root {
					t.Errorf("不变量 Join 在 root 下一层 [形态=%s] name %q: got %q", f.dim, target, joined)
				}
				// Join is lexical: a name that already IS a link in root to
				// elsewhere is outside by Within's definition (see report).
				if !isExistingLink(joined) && !Within(fx.root, joined) {
					t.Errorf("不变量 Join 结果 Within(root) [形态=%s] name %q: %q is not within root", f.dim, target, joined)
				}
			}
		})
	}
}

// Names that ValidateName must refuse even though some of them are one
// harmless-looking path element: each is refused for its own documented
// reason, and Join refuses exactly what ValidateName refuses.
func TestTablePathSafepathNames(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		ok   bool
		why  string
	}{
		{"good-name_1.2", true, "字母数字 - _ ."},
		{strings.Repeat("a", MaxNameLen), true, "恰好上限"},
		{strings.Repeat("a", MaxNameLen+1), false, "超长"},
		{"", false, "空"},
		{".", false, "."},
		{"..", false, ".."},
		{".hidden", false, "开头点"},
		{"name.", false, "尾点：Windows 上等同 name"},
		{"name ", false, "尾空格"},
		{"x.disabled", false, ".disabled 后缀"},
		{"x.disabled.", false, "尾点绕过 .disabled"},
		{"CON", false, "设备名"},
		{"nul.txt", false, "带扩展名的设备名"},
		{"Com1", false, "大小写设备名"},
		{"lpt9.md", false, "LPT9"},
		{"COM0", true, "COM0 不是设备"},
		{"a:b", false, "ADS/盘符冒号"},
		{"a::$DATA", false, "ADS 主流"},
		{"C:", false, "盘符"},
		{"a/b", false, "分隔符 /"},
		{`a\b`, false, `分隔符 \`},
		{`\\host\share`, false, "UNC"},
		{"a\x00b", false, "NUL"},
		{"~", false, "~"},
		{"a b", false, "空格"},
		{"é", false, "非 ASCII 字母"},
		{"LONGDI~1", false, "8.3 短名的 ~"},
		{"file:", false, "URL 形态"},
	}
	for _, c := range cases {
		err := ValidateName("skill", c.name)
		if (err == nil) != c.ok {
			t.Errorf("出口 ValidateName(%q) [%s]: got err=%v, want accepted=%v", c.name, c.why, err, c.ok)
		}
		p, jerr := Join("skill", root, c.name)
		if (jerr == nil) != (err == nil) {
			t.Errorf("出口 Join(root, %q) [%s]: err %v, ValidateName err %v", c.name, c.why, jerr, err)
		}
		if jerr == nil && (filepath.Dir(p) != filepath.Clean(root) || !Within(root, p)) {
			t.Errorf("不变量 Join 在 root 下一层 [%s] name %q: got %q", c.why, c.name, p)
		}
	}
}

// An empty root contains nothing, whatever the target.
func TestTablePathSafepathEmptyRoot(t *testing.T) {
	for _, target := range []string{"", ".", "a.md", string(filepath.Separator)} {
		if Within("", target) {
			t.Errorf("出口 Within(%q, %q) [形态=empty-root]: got true, want false", "", target)
		}
	}
}
