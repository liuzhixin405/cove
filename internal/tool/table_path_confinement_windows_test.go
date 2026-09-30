//go:build windows

package tool

// Windows 专有路径形态（junction、8.3 短名、NTFS ADS、UNC 与 \\?\、盘符、
// Git-Bash 形态、绝对路径大小写），喂给每个文件工具。见
// table_path_confinement_test.go。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func init() {
	platformConfinementSetup = setupWindowsConfinement
	platformConfinementForms = windowsConfinementForms
}

func setupWindowsConfinement(t *testing.T, fx *confFixture) {
	t.Helper()
	fx.junctionWhy = ""
	for _, j := range [][2]string{
		{filepath.Join(fx.root, "sub"), "jin"},
		{fx.outside, "jout"},
		{fx.outside, "junctionlongname"},
	} {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(fx.root, j[1]), j[0]).CombinedOutput(); err != nil {
			fx.junctionWhy = "mklink /J failed: " + err.Error() + " " + string(out)
			break
		}
	}
	other := "Z:"
	if strings.EqualFold(filepath.VolumeName(fx.root), "Z:") {
		other = "Y:"
	}
	fx.repl = append(fx.repl,
		"{vol}", filepath.VolumeName(fx.root),
		"{otherdrive}", other,
		"{sdir}", confShortBase(filepath.Join(fx.root, "longdirectoryname")),
		"{sjunc}", confShortBase(filepath.Join(fx.root, "junctionlongname")),
		"{sroot}", confShortPath(fx.root),
		"{sout}", confShortPath(fx.outside),
	)
}

func confShortPath(p string) string {
	in, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(in, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func confShortBase(p string) string {
	s := confShortPath(p)
	if s == "" || strings.EqualFold(filepath.Base(s), filepath.Base(p)) {
		return ""
	}
	return filepath.Base(s)
}

func windowsConfinementForms(fx *confFixture) []confForm {
	no83 := func(p string) string {
		if confShortBase(p) == "" {
			return "no 8.3 short name on this volume for " + p
		}
		return ""
	}
	no83abs := func(p string) string {
		if s := confShortPath(p); s == "" || strings.EqualFold(s, p) {
			return "no 8.3 spelling differs from " + p
		}
		return ""
	}
	sdir := no83(filepath.Join(fx.root, "longdirectoryname"))
	sjunc := no83(filepath.Join(fx.root, "junctionlongname"))
	sroot := no83abs(fx.root)
	return []confForm{
		// NTFS ADS
		{dim: "ads-named", path: `{f}:x{e}`, want: wantStream, note: "a.md 的隐藏命名流；图片工具得到 a.png:x.png，扩展名检查照样通过"},
		{dim: "ads-data", path: `{f}::$DATA`, want: wantStream, noImage: "a.png::$DATA 的扩展名不是 .png，截图在扩展名检查处就拒绝（不是越界判定）"},
		{dim: "ads-in-dir", path: `sub\{f}:x{e}`, want: wantStream},
		{dim: "ads-abs", path: `{root}\{f}:x{e}`, want: wantStream},
		{dim: "ads-dir-stream", path: `sub::$INDEX_ALLOCATION\{f}`, want: wantStream},
		{dim: "ads-junction-index-allocation", path: `jout::$INDEX_ALLOCATION\{f}`, want: wantStream, needs: "junction", linked: true, note: "已实测：这个形态能穿过 junction 读到外部文件"},
		{dim: "ads-cleaned-away", path: `sub:x\..\{f}`, want: wantInside, note: "Clean 先消去带冒号的段，剩下的是普通工作区文件"},
		{dim: "drive-relative", path: `C:{f}`, want: wantStream, note: "拼到 cwd 后冒号落在盘符之后"},
		{dim: "file-url", path: `file:///C:/{f}`, want: wantStream},
		// 大小写
		{dim: "win-case-abs-inside", path: `{ROOT}\{f}`, want: wantInside, note: "文件系统不区分大小写"},
		{dim: "win-case-abs-outside-sibling", path: `{ROOT}\..\WS2\{f}`, want: wantOutside},
		// junction
		{dim: "junction-inside", path: `jin\{f}`, want: wantInside, needs: "junction", linked: true},
		{dim: "junction-outside-itself", path: `jout`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-outside-file", path: `jout\{f}`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-outside-new", path: `jout\new\{f}`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-outside-case", path: `JOUT\{f}`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-outside-trailing-dot", path: `jout.\{f}`, want: wantOutside, needs: "junction", linked: true, note: `"jout." 由 Win32 规范成 jout`},
		{dim: "junction-outside-trailing-space", path: `jout \{f}`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-outside-abs-case", path: `{ROOT}\JOUT\{f}`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-outside-via-parent", path: `..\ws\jout\{f}`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-then-dotdot", path: `jout\..\{f}`, want: wantInside, needs: "junction", linked: true, note: "Clean 先于解析：按词法回到工作区"},
		{dim: "junction-outside-long", path: `jout\` + strings.Repeat(`d123456789\`, 30) + `{f}`, want: wantOutside, needs: "junction", linked: true},
		{dim: "junction-outside-short-name", path: `{sjunc}\{f}`, want: wantOutside, needs: "junction", linked: true, skip: sjunc},
		{dim: "junction-outside-short-root", path: `{sroot}\jout\{f}`, want: wantOutside, needs: "junction", linked: true, skip: sroot},
		// 8.3 短名
		{dim: "short-dir-inside", path: `{sdir}\{f}`, want: wantInside, skip: sdir},
		{dim: "short-abs-outside", path: `{sout}\{f}`, want: wantOutside, skip: no83abs(fx.outside)},
		{dim: "short-root-abs", path: `{sroot}\{f}`, want: wantAny, skip: sroot, note: "待确认：工作区自身的短名拼写"},
		// UNC、\\?\、\\.\
		{dim: "unc-host-share", path: `\\host\share\{f}`, want: wantOutside},
		{dim: "unc-extended", path: `\\?\UNC\host\share\{f}`, want: wantOutside},
		{dim: "extended-outside", path: `\\?\{out}\{f}`, want: wantOutside},
		{dim: "device-outside", path: `\\.\{out}\{f}`, want: wantOutside},
		{dim: "extended-inside", path: `\\?\{root}\{f}`, want: wantAny, note: "待确认：工作区内文件的 \\\\?\\ 拼写"},
		// 盘符根与其他盘
		{dim: "drive-root", path: `{vol}\`, want: wantOutside},
		{dim: "drive-root-file", path: `{vol}\{f}`, want: wantOutside},
		{dim: "other-drive", path: `{otherdrive}\{f}`, want: wantOutside},
		{dim: "other-drive-lower", path: `z:\{f}`, want: wantOutside, note: "小写盘符（工作区不在 Z: 时）"},
		// Git-Bash 与根相对形态：不是绝对路径，被拼进工作区（已解析路径在工作区内）
		{dim: "gitbash-drive", path: `/c/{f}`, want: wantInside, note: "/c/ 不翻译成 C:\\，落在 cwd\\c\\ 下"},
		{dim: "root-relative", path: `\{f}`, want: wantInside, note: "单个 \\ 开头按 cwd 拼接，解析结果仍在工作区"},
		// 分隔符
		{dim: "win-forward-slash", path: `sub/{f}`, want: wantInside},
		{dim: "win-double-sep", path: `sub\\\{f}`, want: wantInside},
	}
}
