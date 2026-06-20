package repomap

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	mtime time.Time
	fm    FileMap
}

var (
	parseCache   = make(map[string]cacheEntry)
	parseCacheMu sync.RWMutex
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
	Score   int // Rank/importance score based on reference frequency
}

// Generator manages scanning and ranking to produce the Repo Map.
type Generator struct {
	WorkspaceRoot string
	IgnoreDirs    map[string]bool
}

// NewGenerator creates a new Repo Map generator.
func NewGenerator(root string) *Generator {
	return &Generator{
		WorkspaceRoot: root,
		IgnoreDirs: map[string]bool{
			".git":         true,
			"node_modules": true,
			"vendor":       true,
			".github":      true,
			"testdata":     true,
			"build":        true,
			"dist":         true,
		},
	}
}

// Generate scans the directory, extracts definitions, ranks them, and outputs a formatted map.
func (g *Generator) Generate(maxFiles int) string {
	if g.WorkspaceRoot == "" {
		return ""
	}

	var files []string
	err := filepath.Walk(g.WorkspaceRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if g.IgnoreDirs[info.Name()] || strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".go" || ext == ".py" || ext == ".ts" || ext == ".js" {
			files = append(files, path)
		}
		return nil
	})

	if err != nil || len(files) == 0 {
		return ""
	}

	// Phase 1: Parse all files in parallel
	var wg sync.WaitGroup
	resultsChan := make(chan FileMap, len(files))
	for _, fp := range files {
		wg.Add(1)
		go func(filePath string) {
			defer wg.Done()
			fm := g.parseFile(filePath)
			if len(fm.Symbols) > 0 {
				resultsChan <- fm
			}
		}(fp)
	}
	wg.Wait()
	close(resultsChan)

	var fileMaps []FileMap
	for fm := range resultsChan {
		fileMaps = append(fileMaps, fm)
	}

	// Phase 2: Compute core cross-reference rankings to identify highly-referenced definitions
	symbolRefCounts := make(map[string]int)
	// Simple text-based global scan for referencing frequencies
	for _, fm := range fileMaps {
		for _, sym := range fm.Symbols {
			// Find how many times this symbol is mentioned in other files
			for _, otherFm := range fileMaps {
				if otherFm.Path == fm.Path {
					continue
				}
				// Cheap matching trick (whole word or exact substring)
				for _, otherSym := range otherFm.Symbols {
					if strings.Contains(otherSym.Signature, sym.Name) {
						symbolRefCounts[sym.Name]++
					}
				}
			}
		}
	}

	// Apply scores to FileMaps
	for i := range fileMaps {
		score := 0
		for _, sym := range fileMaps[i].Symbols {
			score += symbolRefCounts[sym.Name]
		}
		fileMaps[i].Score = score
	}

	// Sort files: highest scores first (most important symbols defined)
	sort.Slice(fileMaps, func(i, j int) bool {
		return fileMaps[i].Score > fileMaps[j].Score
	})

	// Limit to maxFiles
	if len(fileMaps) > maxFiles {
		fileMaps = fileMaps[:maxFiles]
	}

	// Sort back alphabetically by path for readable output structure
	sort.Slice(fileMaps, func(i, j int) bool {
		return fileMaps[i].Path < fileMaps[j].Path
	})

	// Phase 3: Format output map as a highly compact tree structure
	var sb strings.Builder
	for _, fm := range fileMaps {
		sb.WriteString(fm.Path)
		if fm.Package != "" {
			sb.WriteString(" (package " + fm.Package + ")")
		}
		sb.WriteString(":\n")

		// Group symbols by type to be cleaner
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

// parseFile delegates to AST or Regex-based scanners depending on file extension.
func (g *Generator) parseFile(absPath string) FileMap {
	relPath, err := filepath.Rel(g.WorkspaceRoot, absPath)
	if err != nil {
		relPath = absPath
	}
	relPath = filepath.ToSlash(relPath)

	info, err := os.Stat(absPath)
	var mtime time.Time
	if err == nil {
		mtime = info.ModTime()
		parseCacheMu.RLock()
		cached, exists := parseCache[absPath]
		parseCacheMu.RUnlock()
		if exists && cached.mtime.Equal(mtime) {
			fm := cached.fm
			fm.Path = relPath
			return fm
		}
	}

	ext := strings.ToLower(filepath.Ext(absPath))
	fm := FileMap{Path: relPath}

	if ext == ".go" {
		g.parseGoAST(absPath, &fm)
	} else {
		g.parseRegexBased(absPath, ext, &fm)
	}

	if err == nil {
		parseCacheMu.Lock()
		parseCache[absPath] = cacheEntry{
			mtime: mtime,
			fm:    fm,
		}
		parseCacheMu.Unlock()
	}

	return fm
}

// parseGoAST parses Go source code directly via original Go SDK go/parser.
func (g *Generator) parseGoAST(absPath string, fm *FileMap) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, nil, parser.AllErrors)
	if err != nil {
		return
	}

	fm.Package = file.Name.Name

	ast.Inspect(file, func(n ast.Node) bool {
		switch decl := n.(type) {
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

			// Parse parameters to build a compact but descriptive signature
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
				// Method on struct receiver
				recvField := decl.Recv.List[0]
				recvType := formatGoType(recvField.Type)
				recvName := ""
				if len(recvField.Names) > 0 {
					recvName = recvField.Names[0].Name + " "
				}
				sig := "func (" + recvName + recvType + ") " + name + "(" + paramSpec + ")"
				fm.Symbols = append(fm.Symbols, Symbol{
					Name:      name,
					Type:      "method",
					Signature: sig,
					Line:      line,
				})
			} else {
				// Global function
				sig := "func " + name + "(" + paramSpec + ")"
				fm.Symbols = append(fm.Symbols, Symbol{
					Name:      name,
					Type:      "func",
					Signature: sig,
					Line:      line,
				})
			}
		}
		return true
	})
}

// parseRegexBased extracts classes and methods from python/typescript/javascript folders with lightweight patterns.
func (g *Generator) parseRegexBased(absPath string, ext string, fm *FileMap) {
	file, err := os.Open(absPath)
	if err != nil {
		return
	}
	defer file.Close()

	var patterns []*regexp.Regexp
	if ext == ".py" {
		// Python patterns: class, def
		patterns = []*regexp.Regexp{
			regexp.MustCompile(`^\s*(class\s+([a-zA-Z0-9_]+)\s*(\([a-zA-Z0-9_,\s]*\))?:)`),
			regexp.MustCompile(`^\s*(def\s+([a-zA-Z0-9_]+)\s*\((.*?)\):)`),
		}
	} else if ext == ".ts" || ext == ".js" {
		// TypeScript/JS patterns: export class, function, interface, export function
		patterns = []*regexp.Regexp{
			regexp.MustCompile(`^\s*(?:export\s+)?(?:class)\s+([a-zA-Z0-9_]+)`),
			regexp.MustCompile(`^\s*(?:export\s+)?(?:interface)\s+([a-zA-Z0-9_]+)`),
			regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?(?:function)\s+([a-zA-Z0-9_]+)\s*\((.*?)\)`),
		}
	}

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}

		for _, pat := range patterns {
			matches := pat.FindStringSubmatch(line)
			if len(matches) > 0 {
				fullMatch := strings.TrimSpace(matches[0])
				// Clean trailing brackets or colons
				fullMatch = strings.TrimSuffix(fullMatch, ":")
				fullMatch = strings.TrimSuffix(fullMatch, " {")

				name := ""
				if len(matches) > 1 {
					name = matches[1]
				}
				typ := "definition"
				if strings.Contains(fullMatch, "class") {
					typ = "class"
				} else if strings.Contains(fullMatch, "def") || strings.Contains(fullMatch, "function") {
					typ = "func"
				} else if strings.Contains(fullMatch, "interface") {
					typ = "interface"
				}

				fm.Symbols = append(fm.Symbols, Symbol{
					Name:      name,
					Type:      typ,
					Signature: fullMatch,
					Line:      lineNum,
				})
				break
			}
		}
	}
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
	default:
		return "any"
	}
}
