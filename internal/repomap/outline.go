package repomap

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// OutlineMaxBytes caps the project outline (tags included). The outline sits
// in the system prompt of every request, so it only names the project's
// shape; symbols come from the repo_map tool (Query).
const OutlineMaxBytes = 4096

// outlineMaxEntries bounds the walk behind Outline on very large trees.
const outlineMaxEntries = 60000

// outlineSkipDirs are never descended into, on top of every dot-directory.
var outlineSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true, "build": true, "dist": true,
	"target": true, "bin": true, "obj": true, "__pycache__": true, "venv": true,
}

// languageByExt names the languages the outline counts.
var languageByExt = map[string]string{
	".go": "Go", ".py": "Python", ".ts": "TypeScript", ".tsx": "TypeScript",
	".js": "JavaScript", ".jsx": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript",
	".rs": "Rust", ".java": "Java", ".kt": "Kotlin", ".c": "C", ".h": "C",
	".cpp": "C++", ".cc": "C++", ".hpp": "C++", ".cs": "C#", ".rb": "Ruby",
	".php": "PHP", ".swift": "Swift", ".md": "Markdown", ".sh": "Shell",
	".ps1": "PowerShell", ".sql": "SQL", ".vue": "Vue", ".html": "HTML", ".css": "CSS",
}

// entryFileNames are file names that usually start a program.
var entryFileNames = map[string]bool{
	"main.go": true, "main.py": true, "__main__.py": true, "app.py": true, "manage.py": true,
	"main.ts": true, "index.ts": true, "main.js": true, "index.js": true, "main.rs": true,
	"Program.cs": true, "Main.java": true,
}

type outlineScan struct {
	topDirs   map[string]int // top-level dir -> files under it
	topFiles  []string       // files directly in root
	langs     map[string]int // language -> files
	packages  map[string]int // "a/b" -> source files at or below it
	entries   []string       // likely entry points
	rootFiles map[string]bool
	mapped    int // files repo_map parses (isScannedExt)
}

// Outline describes the project at root in at most OutlineMaxBytes: the
// languages by file count, the top-level directories and files, the
// second-level source directories, likely entry points and the build/test
// commands its build files imply. The output is sorted and carries no
// timestamps, so it is stable across turns (it sits in the cached system
// prompt). Empty when root cannot be read.
func Outline(root string) string {
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return ""
	}
	sc := scanOutline(root)

	// The hint goes first so a cut at the cap never drops it.
	lines := []string{"File symbols are not listed here: call the repo_map tool (query by path or identifier), grep or glob when you need them."}
	if sc.mapped == 0 {
		lines[0] = "File symbols are not listed here. repo_map only maps Go, Python, TypeScript and JavaScript, none of which is here: use grep and glob to explore the code."
	}
	if l := languagesLine(sc.langs); l != "" {
		lines = append(lines, l)
	}
	if l := topLevelLine(sc); l != "" {
		lines = append(lines, l)
	}
	if l := packagesLine(sc.packages); l != "" {
		lines = append(lines, l)
	}
	if len(sc.entries) > 0 {
		sort.Strings(sc.entries)
		lines = append(lines, "Entry points: "+joinCapped(sc.entries, 500))
	}
	if cmds := buildCommands(root, sc.rootFiles); len(cmds) > 0 {
		lines = append(lines, "Build/test: "+joinCappedSep(cmds, " | ", 500))
	}
	if len(lines) == 1 {
		return ""
	}

	const open, closing = "<project_outline>\n", "</project_outline>\n"
	body := strings.Join(lines, "\n") + "\n"
	if limit := OutlineMaxBytes - len(open) - len(closing); len(body) > limit {
		body = cutAtLine(body, limit)
	}
	return open + body + closing
}

func scanOutline(root string) *outlineScan {
	sc := &outlineScan{
		topDirs:   map[string]int{},
		langs:     map[string]int{},
		packages:  map[string]int{},
		rootFiles: map[string]bool{},
	}
	seen := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		seen++
		if seen > outlineMaxEntries {
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || outlineSkipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		parts := strings.Split(rel, "/")
		if len(parts) == 1 {
			sc.topFiles = append(sc.topFiles, name)
			sc.rootFiles[name] = true
		} else {
			sc.topDirs[parts[0]]++
		}
		ext := strings.ToLower(path.Ext(name))
		if isScannedExt(ext) {
			sc.mapped++
		}
		lang, known := languageByExt[ext]
		if known {
			sc.langs[lang]++
		}
		if known && lang != "Markdown" && len(parts) >= 3 {
			sc.packages[parts[0]+"/"+parts[1]]++
		}
		if entryFileNames[name] && len(parts) <= 4 {
			sc.entries = append(sc.entries, rel)
		}
		return nil
	})
	return sc
}

func languagesLine(langs map[string]int) string {
	if len(langs) == 0 {
		return ""
	}
	names := make([]string, 0, len(langs))
	for n := range langs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if langs[names[i]] != langs[names[j]] {
			return langs[names[i]] > langs[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > 8 {
		names = names[:8]
	}
	items := make([]string, len(names))
	for i, n := range names {
		items[i] = fmt.Sprintf("%s (%d files)", n, langs[n])
	}
	return "Languages: " + strings.Join(items, ", ")
}

func topLevelLine(sc *outlineScan) string {
	dirs := make([]string, 0, len(sc.topDirs))
	for d := range sc.topDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	items := make([]string, len(dirs))
	for i, d := range dirs {
		items[i] = fmt.Sprintf("%s/ (%d)", d, sc.topDirs[d])
	}
	sort.Strings(sc.topFiles)
	var lines []string
	if len(items) > 0 {
		lines = append(lines, "Top-level dirs (files): "+joinCapped(items, 900))
	}
	if len(sc.topFiles) > 0 {
		lines = append(lines, "Top-level files: "+joinCapped(sc.topFiles, 500))
	}
	return strings.Join(lines, "\n")
}

func packagesLine(pkgs map[string]int) string {
	if len(pkgs) == 0 {
		return ""
	}
	names := make([]string, 0, len(pkgs))
	for n := range pkgs {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]string, len(names))
	for i, n := range names {
		items[i] = fmt.Sprintf("%s (%d)", n, pkgs[n])
	}
	return "Source dirs (files): " + joinCapped(items, 1800)
}

var makeTarget = regexp.MustCompile(`(?m)^([A-Za-z0-9][A-Za-z0-9_.-]*)\s*:([^=]|$)`)

// buildCommands derives build/test commands from the build files in root.
func buildCommands(root string, rootFiles map[string]bool) []string {
	var cmds []string
	if rootFiles["go.mod"] {
		cmds = append(cmds, "go build ./...", "go test ./...", "go vet ./...")
	}
	if rootFiles["package.json"] {
		cmds = append(cmds, npmCommands(filepath.Join(root, "package.json"))...)
	}
	if rootFiles["Cargo.toml"] {
		cmds = append(cmds, "cargo build", "cargo test")
	}
	if rootFiles["pyproject.toml"] || rootFiles["setup.py"] || rootFiles["requirements.txt"] {
		cmds = append(cmds, "pytest")
	}
	if rootFiles["pom.xml"] {
		cmds = append(cmds, "mvn package", "mvn test")
	}
	if rootFiles["build.gradle"] || rootFiles["build.gradle.kts"] {
		cmds = append(cmds, "gradle build", "gradle test")
	}
	names := make([]string, 0, len(rootFiles))
	for n := range rootFiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasSuffix(n, ".sln") || strings.HasSuffix(n, ".csproj") {
			cmds = append(cmds, "dotnet build", "dotnet test")
			break
		}
	}
	if rootFiles["CMakeLists.txt"] {
		cmds = append(cmds, "cmake -B build && cmake --build build")
	}
	if rootFiles["Makefile"] {
		cmds = append(cmds, makeCommand(filepath.Join(root, "Makefile")))
	}
	return cmds
}

func makeCommand(makefile string) string {
	data, err := os.ReadFile(makefile)
	if err != nil {
		return "make"
	}
	var targets []string
	seen := map[string]bool{}
	for _, m := range makeTarget.FindAllStringSubmatch(string(data), -1) {
		if !seen[m[1]] && len(targets) < 10 {
			seen[m[1]] = true
			targets = append(targets, m[1])
		}
	}
	if len(targets) == 0 {
		return "make"
	}
	return "make (" + strings.Join(targets, ", ") + ")"
}

func npmCommands(pkgJSON string) []string {
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	var cmds []string
	for _, s := range []string{"build", "test", "lint", "typecheck", "dev"} {
		if _, ok := pkg.Scripts[s]; !ok {
			continue
		}
		if s == "test" {
			cmds = append(cmds, "npm test")
		} else {
			cmds = append(cmds, "npm run "+s)
		}
	}
	return cmds
}

// joinCapped joins items with ", " within limit bytes, ending with a count
// of the items left out.
func joinCapped(items []string, limit int) string { return joinCappedSep(items, ", ", limit) }

func joinCappedSep(items []string, sep string, limit int) string {
	var sb strings.Builder
	for i, it := range items {
		add := it
		if i > 0 {
			add = sep + it
		}
		last := i == len(items)-1
		more := fmt.Sprintf("%s… +%d more", sep, len(items)-i)
		if (last && sb.Len()+len(add) > limit) || (!last && sb.Len()+len(add)+len(more) > limit) {
			if i == 0 {
				return fmt.Sprintf("… %d items", len(items))
			}
			sb.WriteString(more)
			return sb.String()
		}
		sb.WriteString(add)
	}
	return sb.String()
}

// cutAtLine cuts s to at most limit bytes at a line boundary.
func cutAtLine(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return ""
}
