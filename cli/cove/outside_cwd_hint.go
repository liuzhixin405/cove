package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// absDirRe finds absolute paths in a request: a Windows drive path, or a
// Unix path with at least two components.
var absDirRe = regexp.MustCompile(`(?i)\b[a-z]:[/\\][^\s"'<>|*?，。；：]+|(?:^|\s)/[^\s"'<>|*?，。；：/]+(?:/[^\s"'<>|*?，。；：/]+)+`)

// outsideCwdHint returns the line to show before submitting text when it
// names an existing directory (or a file in one) outside cwd, or "" when it
// does not. The file tools refuse such paths; a real session spent 90
// minutes with the model trying bash and powershell around the refusal
// while the person, who could have fixed it with /cd, saw only "工具 write
// 失败".
func outsideCwdHint(text, cwd string) string {
	cwd = cleanDir(cwd)
	if cwd == "" {
		return ""
	}
	for _, m := range absDirRe.FindAllString(text, -1) {
		p := filepath.Clean(strings.TrimSpace(m))
		dir := existingDir(p)
		if dir == "" || sameOrInside(dir, cwd) {
			continue
		}
		return "  \x1b[33m⚠ 请求里的目录 " + dir + " 在当前工作目录（" + cwd + "）之外，cove 的文件工具只在工作目录内读写。要在那里工作，先输入 /cd " + dir + " 再发请求。\x1b[0m"
	}
	return ""
}

// existingDir returns p when it is an existing directory, its parent when
// p is (or would be) a file in an existing directory, else "".
func existingDir(p string) string {
	if info, err := os.Stat(p); err == nil {
		if info.IsDir() {
			return p
		}
		return filepath.Dir(p)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return ""
	}
	if info, err := os.Stat(parent); err == nil && info.IsDir() {
		return parent
	}
	return ""
}

func cleanDir(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return ""
	}
	if abs, err := filepath.Abs(d); err == nil {
		d = abs
	}
	return filepath.Clean(d)
}

func sameOrInside(dir, root string) bool {
	if runtime.GOOS == "windows" {
		dir, root = strings.ToLower(dir), strings.ToLower(root)
	}
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}
