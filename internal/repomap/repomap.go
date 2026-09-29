package repomap

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Symbol represents a definition found in the code (struct, interface, function).
type Symbol struct {
	Name      string
	Type      string // "struct", "interface", "func", "method", "class", "def"
	Signature string // Simplified signature for LLM context
	Line      int
}

// FileMap represents the code map of a single file.
type FileMap struct {
	Path    string // relative path
	Package string // package name (mainly for Go)
	Symbols []Symbol
	Score   int // PageRank of the file in the reference graph, x1e6
}

// Generator produces the ranked repo map of a workspace. It is a thin view
// over the workspace's shared Index (IndexFor), so every caller — the
// /context command, the repo_map tool, the per-turn excerpt — reuses the same
// incremental parse state instead of re-parsing the tree.
type Generator struct {
	WorkspaceRoot string
}

// NewGenerator creates a new Repo Map generator.
func NewGenerator(root string) *Generator {
	return &Generator{WorkspaceRoot: root}
}

// Generate returns the formatted map of the maxFiles highest-ranked files.
func (g *Generator) Generate(maxFiles int) string {
	return FormatFileMaps(g.BuildRanked(maxFiles))
}

// BuildRanked brings the workspace index up to date (only files whose mtime
// or size changed are re-parsed), then returns the maxFiles files with the
// highest reference-graph rank, re-sorted into path order for stable,
// readable output.
func (g *Generator) BuildRanked(maxFiles int) []FileMap {
	if g.WorkspaceRoot == "" {
		return nil
	}
	ix := IndexFor(g.WorkspaceRoot)
	ix.Refresh()
	fileMaps := ix.Files()

	// Highest rank first; ties are broken by path so the cut at maxFiles
	// picks the same files on every run.
	sort.SliceStable(fileMaps, func(i, j int) bool {
		if fileMaps[i].Score != fileMaps[j].Score {
			return fileMaps[i].Score > fileMaps[j].Score
		}
		return fileMaps[i].Path < fileMaps[j].Path
	})
	if len(fileMaps) > maxFiles {
		fileMaps = fileMaps[:maxFiles]
	}
	sort.Slice(fileMaps, func(i, j int) bool { return fileMaps[i].Path < fileMaps[j].Path })
	return fileMaps
}

// isScannedExt reports whether files with extension ext (lower-case, with the
// dot) are parsed for symbols.
func isScannedExt(ext string) bool {
	if ext == ".go" {
		return true
	}
	_, ok := regexLangs[ext]
	return ok
}

// ScannedLanguages names the languages the repo map parses, for tool
// descriptions and hints.
const ScannedLanguages = "Go, Python, TypeScript, JavaScript, C#, Java and Rust"

// FormatFileMaps renders a compact, LLM-friendly text map from already
// ranked/selected FileMaps, grouping symbols under each file's path/package.
func FormatFileMaps(fileMaps []FileMap) string {
	var sb strings.Builder
	for _, fm := range fileMaps {
		sb.WriteString(fm.Path)
		if fm.Package != "" {
			sb.WriteString(" (package " + fm.Package + ")")
		}
		sb.WriteString(":\n")
		for _, sym := range fm.Symbols {
			prefix := "  - "
			if sym.Type == "method" {
				prefix = "    "
			}
			sb.WriteString(prefix + sym.Signature + "\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// minRefIdentLen: identifiers shorter than this ("i", "ok", "db") are too
// common to say anything about which file depends on which.
const minRefIdentLen = 3

// parseFile extracts the definitions of the file at absPath (relPath is
// already slash-separated) and the identifiers it references, with counts.
func parseFile(absPath, relPath, ext string) (FileMap, map[string]int) {
	fm := FileMap{Path: relPath}
	if ext == ".go" {
		return fm, parseGoAST(absPath, &fm)
	}
	return fm, parseRegexBased(absPath, ext, &fm)
}

// parseGoAST parses Go source with go/parser: type and func declarations
// become symbols, and every identifier in the file counts as a reference.
func parseGoAST(absPath string, fm *FileMap) map[string]int {
	fset := token.NewFileSet()
	// A file with syntax errors still yields the declarations before them
	// (a half-edited file keeps its place in the map), so the error is not
	// fatal.
	file, _ := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
	if file == nil || file.Name == nil {
		return nil
	}
	fm.Package = file.Name.Name
	refs := map[string]int{}

	ast.Inspect(file, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.Ident:
			if len(decl.Name) >= minRefIdentLen {
				refs[decl.Name]++
			}
		case *ast.TypeSpec:
			line := fset.Position(decl.Pos()).Line
			switch decl.Type.(type) {
			case *ast.StructType:
				fm.Symbols = append(fm.Symbols, Symbol{
					Name:      decl.Name.Name,
					Type:      "struct",
					Signature: "type " + decl.Name.Name + " struct",
					Line:      line,
				})
			case *ast.InterfaceType:
				fm.Symbols = append(fm.Symbols, Symbol{
					Name:      decl.Name.Name,
					Type:      "interface",
					Signature: "type " + decl.Name.Name + " interface",
					Line:      line,
				})
			}

		case *ast.FuncDecl:
			line := fset.Position(decl.Pos()).Line
			name := decl.Name.Name

			var params []string
			if decl.Type.Params != nil {
				for _, field := range decl.Type.Params.List {
					typeName := formatGoType(field.Type)
					if len(field.Names) > 0 {
						for _, n := range field.Names {
							params = append(params, n.Name+" "+typeName)
						}
					} else {
						params = append(params, typeName)
					}
				}
			}
			paramSpec := strings.Join(params, ", ")

			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				recvField := decl.Recv.List[0]
				recvType := formatGoType(recvField.Type)
				recvName := ""
				if len(recvField.Names) > 0 {
					recvName = recvField.Names[0].Name + " "
				}
				fm.Symbols = append(fm.Symbols, Symbol{
					Name:      name,
					Type:      "method",
					Signature: "func (" + recvName + recvType + ") " + name + "(" + paramSpec + ")",
					Line:      line,
				})
			} else {
				fm.Symbols = append(fm.Symbols, Symbol{
					Name:      name,
					Type:      "func",
					Signature: "func " + name + "(" + paramSpec + ")",
					Line:      line,
				})
			}
		}
		return true
	})
	// A file's own declarations name themselves; they are not references.
	for _, s := range fm.Symbols {
		if refs[s.Name] > 0 {
			refs[s.Name]--
		}
	}
	return refs
}

// defPattern is one kind of definition a regex scanner recognises: group 1
// is the name, and the whole match (trimmed) is the signature.
type defPattern struct {
	re  *regexp.Regexp
	typ string
}

// regexLang is how one language's files are scanned without a parser.
type regexLang struct {
	defs        []defPattern
	lineComment string // prefix of a whole-line comment
	// indentedMethod: an indented function definition is a method (Python).
	indentedMethod bool
}

var (
	pyLang = regexLang{
		lineComment:    "#",
		indentedMethod: true,
		defs: []defPattern{
			{regexp.MustCompile(`^\s*class\s+([A-Za-z0-9_]+)\s*(?:\([^)]*\))?\s*:`), "class"},
			{regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z0-9_]+)\s*\((.*?)\)(?:\s*->\s*[^:]+)?\s*:`), "func"},
		},
	}
	jsLang = regexLang{
		lineComment: "//",
		defs: []defPattern{
			{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z0-9_$]+)`), "class"},
			{regexp.MustCompile(`^\s*(?:export\s+)?interface\s+([A-Za-z0-9_$]+)`), "interface"},
			{regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?(?:type)\s+([A-Za-z0-9_$]+)\s*(?:<[^=]*>)?\s*=`), "type"},
			{regexp.MustCompile(`^\s*(?:export\s+)?(?:const\s+)?enum\s+([A-Za-z0-9_$]+)`), "enum"},
			{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z0-9_$]+)\s*(?:<[^>]*>)?\s*\((.*?)\)`), "func"},
			{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let)\s+([A-Za-z0-9_$]+)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)|[A-Za-z0-9_$]+)\s*(?::[^=]+)?=>`), "func"},
		},
	}
	// csMods / javaMods are the modifiers a member declaration starts
	// with; requiring one keeps statements such as "return Foo(x);" out.
	csMods = `(?:(?:public|private|protected|internal|static|virtual|override|abstract|async|sealed|partial|readonly|extern|unsafe|new)\s+)`
	csLang = regexLang{
		lineComment: "//",
		defs: []defPattern{
			{regexp.MustCompile(`^\s*` + csMods + `*(?:record\s+(?:class|struct)|class|interface|struct|record|enum)\s+([A-Za-z0-9_]+)`), "class"},
			{regexp.MustCompile(`^\s*` + csMods + `+[A-Za-z0-9_<>\[\],.? ]+?\s+([A-Za-z0-9_]+)\s*(?:<[^>()]*>)?\s*\(([^)]*)\)?`), "method"},
		},
	}
	javaMods = `(?:(?:public|private|protected|static|final|abstract|synchronized|default|native)\s+)`
	javaLang = regexLang{
		lineComment: "//",
		defs: []defPattern{
			{regexp.MustCompile(`^\s*` + javaMods + `*(?:class|interface|enum|record|@interface)\s+([A-Za-z0-9_]+)`), "class"},
			{regexp.MustCompile(`^\s*` + javaMods + `+(?:<[^>]+>\s+)?[A-Za-z0-9_<>\[\],.? ]+?\s+([A-Za-z0-9_]+)\s*\(([^)]*)\)?`), "method"},
		},
	}
	rustLang = regexLang{
		lineComment: "//",
		defs: []defPattern{
			{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|union)\s+([A-Za-z0-9_]+)`), "struct"},
			{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?type\s+([A-Za-z0-9_]+)`), "type"},
			{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+([A-Za-z0-9_]+)\s*(?:<[^>]*>)?\s*\(([^)]*)\)?`), "func"},
		},
	}
)

// regexLangs maps the extensions scanned without a parser to their scanner.
var regexLangs = map[string]*regexLang{
	".py": &pyLang,
	".ts": &jsLang, ".tsx": &jsLang, ".js": &jsLang, ".jsx": &jsLang, ".mjs": &jsLang, ".cjs": &jsLang,
	".cs":   &csLang,
	".java": &javaLang,
	".rs":   &rustLang,
}

var identRe = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*`)

// maxScanLine: longer lines (minified bundles, data blobs) are skipped.
const maxScanLine = 4096

// parseRegexBased extracts definitions with the language's patterns and
// counts every identifier outside whole-line comments as a reference.
func parseRegexBased(absPath string, ext string, fm *FileMap) map[string]int {
	lang := regexLangs[ext]
	if lang == nil {
		return nil
	}
	file, err := os.Open(absPath)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()

	refs := map[string]int{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || len(line) > maxScanLine || strings.HasPrefix(trimmed, lang.lineComment) ||
			strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		for _, id := range identRe.FindAllString(line, -1) {
			if len(id) >= minRefIdentLen {
				refs[id]++
			}
		}
		for _, p := range lang.defs {
			m := p.re.FindStringSubmatch(line)
			if m == nil || m[1] == "" {
				continue
			}
			sig := strings.TrimSpace(m[0])
			sig = strings.TrimSuffix(sig, ":")
			sig = strings.TrimSuffix(sig, "{")
			sig = strings.TrimSpace(strings.TrimSuffix(sig, "=>"))
			typ := p.typ
			if lang.indentedMethod && typ == "func" && line != trimmed {
				typ = "method"
			}
			fm.Symbols = append(fm.Symbols, Symbol{Name: m[1], Type: typ, Signature: sig, Line: lineNum})
			if refs[m[1]] > 0 {
				refs[m[1]]--
			}
			break
		}
	}
	return refs
}

// formatGoType converts ast.Expr to its highly readable compact string format (pointer *, arrays [], selectors, name maps)
func formatGoType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + formatGoType(t.X)
	case *ast.ArrayType:
		return "[]" + formatGoType(t.Elt)
	case *ast.SelectorExpr:
		return formatGoType(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map[" + formatGoType(t.Key) + "]" + formatGoType(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.Ellipsis:
		return "..." + formatGoType(t.Elt)
	case *ast.FuncType:
		return "func(...)"
	case *ast.ChanType:
		return "chan " + formatGoType(t.Value)
	case *ast.IndexExpr:
		return formatGoType(t.X) + "[" + formatGoType(t.Index) + "]"
	default:
		return "any"
	}
}
