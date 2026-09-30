//go:build windows

package safepath

// Windows 专有路径形态：junction（mklink /J 不需要特权）、8.3 短名、NTFS ADS、
// 绝对路径的大小写差异、尾部点/空格落在 junction 上、UNC 与 \\?\ 前缀、
// 盘符根与其他盘、Git-Bash 形态。见 table_safepath_test.go。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func init() {
	platformWithinSetup = setupWindowsWithin
	platformWithinForms = windowsWithinForms
}

func setupWindowsWithin(t *testing.T, fx *withinFixture) {
	t.Helper()
	fx.junctionWhy = ""
	for _, j := range [][2]string{
		{filepath.Join(fx.root, "sub"), "jin"},
		{fx.outside, "jout"},
		{fx.outside, "junctionlongname"},
	} {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(fx.root, j[1]), j[0]).CombinedOutput(); err != nil {
			fx.junctionWhy = "mklink /J failed: " + err.Error() + " " + string(out)
			return
		}
	}
}

// shortPath is GetShortPathName(p); "" when the volume makes no 8.3 names.
func shortPath(p string) string {
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

// shortBase is the 8.3 name of the last element of p, or "" when there is none.
func shortBase(p string) string {
	s := shortPath(p)
	if s == "" || strings.EqualFold(filepath.Base(s), filepath.Base(p)) {
		return ""
	}
	return filepath.Base(s)
}

// otherDrive is a drive letter that is not root's.
func otherDrive(root string) string {
	if strings.EqualFold(filepath.VolumeName(root), "Z:") {
		return "Y:"
	}
	return "Z:"
}

func windowsWithinForms(fx *withinFixture) []withinForm {
	vol := filepath.VolumeName(fx.root)
	upperRoot := strings.ToUpper(fx.root)
	shortDir := shortBase(filepath.Join(fx.root, "longdirectoryname"))
	shortFile := shortBase(filepath.Join(fx.root, "verylongfilename.md"))
	shortJunction := shortBase(filepath.Join(fx.root, "junctionlongname"))
	shortOutside := shortPath(fx.outside)
	shortRoot := shortPath(fx.root)
	no83 := func(s string) string {
		if s == "" {
			return "this volume has no 8.3 short name for the fixture (8dot3name disabled?)"
		}
		return ""
	}
	return []withinForm{
		// 绝对路径大小写差异
		{dim: "win-case-abs-inside", target: func(*withinFixture) string { return upperRoot + `\A.MD` }, want: true, note: "Rel 区分大小写，Windows 文件系统不区分"},
		{dim: "win-case-abs-prefix-sibling", target: func(fx *withinFixture) string { return strings.ToUpper(filepath.Join(fx.base, "ws2", "a.md")) }, want: false},
		// junction
		{dim: "junction-inside", target: rel(`jin\b.md`), want: true, needs: "junction", linked: true},
		{dim: "junction-outside-itself", target: rel(`jout`), want: false, needs: "junction", linked: true, nameOK: true},
		{dim: "junction-outside-file", target: rel(`jout\secret.md`), want: false, needs: "junction", linked: true},
		{dim: "junction-outside-new", target: rel(`jout\new\x.md`), want: false, needs: "junction", linked: true, note: "junction 下的新文件"},
		{dim: "junction-outside-case", target: rel(`JOUT\secret.md`), want: false, needs: "junction", linked: true, note: "大小写不同的 junction 名"},
		{dim: "junction-outside-trailing-dot", target: rel(`jout.\secret.md`), want: false, needs: "junction", linked: true, note: `"jout." 由 Win32 规范成 jout，能读到外部文件（已实测）`},
		{dim: "junction-outside-trailing-space", target: rel(`jout \secret.md`), want: false, needs: "junction", linked: true, note: "Lstat 把 \"jout \" 报成 junction"},
		{dim: "junction-outside-index-allocation", target: rel(`jout::$INDEX_ALLOCATION\secret.md`), want: false, needs: "junction", linked: true, note: "目录的 ::$INDEX_ALLOCATION 流仍穿过 junction（已实测能读到外部文件）"},
		{dim: "junction-outside-abs-case", target: func(*withinFixture) string { return upperRoot + `\JOUT\secret.md` }, want: false, needs: "junction", linked: true},
		{dim: "junction-outside-via-parent", target: rel(`..\ws\jout\secret.md`), want: false, needs: "junction", linked: true},
		{dim: "junction-then-dotdot", target: rel(`jout\..\a.md`), want: true, needs: "junction", linked: true, note: "Clean 先于解析：jout\\.. 按词法消去，Win32 也按词法处理"},
		{dim: "junction-outside-long", target: rel(`jout\` + strings.Repeat(`d123456789\`, 30) + "x.md"), want: false, needs: "junction", linked: true, note: "总长 > 260 仍解析 junction"},
		{dim: "junction-outside-short-name", target: func(*withinFixture) string { return shortJunction + `\secret.md` }, want: false, needs: "junction", linked: true, skip: no83(shortJunction), note: "junction 的 8.3 短名"},
		{dim: "junction-outside-short-root", target: func(*withinFixture) string { return shortRoot + `\jout\secret.md` }, want: false, needs: "junction", linked: true, skip: no83(shortRoot)},
		// 8.3 短名
		{dim: "short-dir-inside", target: func(*withinFixture) string { return shortDir + `\c.md` }, want: true, skip: no83(shortDir)},
		{dim: "short-file-inside", target: func(*withinFixture) string { return shortFile }, want: true, skip: no83(shortFile)},
		{dim: "short-abs-outside", target: func(*withinFixture) string { return shortOutside + `\secret.md` }, want: false, skip: no83(shortOutside)},
		// NTFS ADS：Within 不管流，只判定位置；拒绝流是 resolvePathInCwd 的事
		{dim: "ads-named", target: rel(`a.md:x`), want: true, note: "a.md 的命名流仍在 root 内"},
		{dim: "ads-data", target: rel(`a.md::$DATA`), want: true},
		{dim: "ads-then-escape", target: rel(`sub:x\..\..\out\secret.md`), want: false},
		// 分隔符
		{dim: "win-forward-slash", target: raw(`sub/b.md`), want: true},
		{dim: "win-double-sep", target: raw(`sub\\\b.md`), want: true},
		// UNC 与设备前缀
		{dim: "unc-host-share", target: raw(`\\host\share\x`), want: false, note: "另一台机器"},
		{dim: "unc-extended", target: raw(`\\?\UNC\host\share\x`), want: false},
		{dim: "extended-outside", target: func(fx *withinFixture) string { return `\\?\` + filepath.Join(fx.outside, "secret.md") }, want: false},
		{dim: "device-outside", target: func(fx *withinFixture) string { return `\\.\` + filepath.Join(fx.outside, "secret.md") }, want: false},
		// 待确认：拼写不同但指向工作区内的形态，以及 OS 与 Go 对“绝对”理解不同的形态
		{dim: "unsettled-extended-inside", target: func(fx *withinFixture) string { return `\\?\` + filepath.Join(fx.root, "a.md") }, unsettled: true, note: `工作区内文件的 \\?\ 拼写`},
		{dim: "unsettled-short-root", target: func(*withinFixture) string { return shortRoot + `\a.md` }, unsettled: true, skip: no83(shortRoot), note: "工作区自身的 8.3 拼写"},
		{dim: "unsettled-root-relative", target: raw(`\Windows\win.ini`), unsettled: true, note: "Go 不认为是绝对路径，按 root 拼接；OS 直接打开时是盘根下"},
		{dim: "unsettled-drive-relative", target: func(*withinFixture) string { return vol + `a.md` }, unsettled: true, note: "C:a.md：OS 按该盘的当前目录解析"},
		// 盘符
		{dim: "drive-root", target: func(*withinFixture) string { return vol + `\` }, want: false},
		{dim: "drive-root-file", target: func(*withinFixture) string { return vol + `\Windows\win.ini` }, want: false},
		{dim: "other-drive", target: func(fx *withinFixture) string { return otherDrive(fx.root) + `\x.md` }, want: false},
		{dim: "other-drive-lower", target: func(fx *withinFixture) string { return strings.ToLower(otherDrive(fx.root)) + `\x.md` }, want: false},
	}
}
