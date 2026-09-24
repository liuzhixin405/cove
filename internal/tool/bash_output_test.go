package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/shell"
)

func TestBashLongOutputKeepsTheEnd(t *testing.T) {
	if shell.Default().Kind != shell.Bash {
		t.Skip("needs a bash shell")
	}
	cmd := `for i in $(seq 1 3000); do echo "ok  line $i of the build log"; done; echo "FINAL: 2 tests failed"`
	res, err := NewBashTool().Call(context.Background(), Input{"command": cmd}, Context{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Data, "ok  line 1 of") {
		t.Error("output lost its start")
	}
	if !strings.Contains(res.Data, "FINAL: 2 tests failed") {
		t.Error("output lost its last line")
	}
	if !strings.Contains(res.Data, "omitted") {
		t.Error("no marker for the dropped middle")
	}
}
