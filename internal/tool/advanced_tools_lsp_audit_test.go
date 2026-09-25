package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeGoModule(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	// GOWORK, GOTOOLCHAIN and GOFLAGS are set for the package by TestMain,
	// so these tests can run in parallel (t.Setenv forbids that).
	dir := t.TempDir()
	files["go.mod"] = "module example.com/diag\n\ngo 1.21\n"
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// lsp is read-only and auto-allowed (also in plan mode, and for read-only
// sub-agents), yet with no LSP backend its "diagnostics" ran `go test ./...`,
// which executes the project's test code — anything at all — unprompted.
func TestLSPDiagnosticsDoesNotExecuteProjectCode(t *testing.T) {
	t.Parallel() // runs the go toolchain: seconds, spent alongside other slow tests
	marker := filepath.Join(t.TempDir(), "RAN")
	dir := writeGoModule(t, map[string]string{
		"lib.go": "package diag\n\nfunc Add(a, b int) int { return a + b }\n",
		"lib_test.go": "package diag\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestMain(m *testing.M) {\n\t_ = os.WriteFile(`" + marker + "`, nil, 0o644)\n\tos.Exit(m.Run())\n}\n",
	})

	res, _ := NewLSPTool().Call(context.Background(),
		Input{"action": "diagnostics", "filePath": filepath.Join(dir, "lib.go")}, Context{})
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("diagnostics executed the project's test code (result: %q)", res.Data)
	}
	if res.IsError {
		t.Fatalf("diagnostics on a clean package failed: %q", res.Data)
	}
}

func TestLSPDiagnosticsReportsCompileErrors(t *testing.T) {
	t.Parallel()
	dir := writeGoModule(t, map[string]string{
		"lib.go": "package diag\n\nfunc Add(a, b int) int { return undefinedName }\n",
	})
	res, _ := NewLSPTool().Call(context.Background(),
		Input{"action": "diagnostics", "filePath": filepath.Join(dir, "lib.go")}, Context{})
	if !res.IsError || !strings.Contains(res.Data, "undefinedName") {
		t.Fatalf("want a diagnostic naming undefinedName, got IsError=%v %q", res.IsError, res.Data)
	}
}
