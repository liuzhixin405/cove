package tool

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// Windows PowerShell writes to a pipe in the console's OEM code page (GBK on
// a Chinese system), so non-ASCII output reached the model as mojibake.
func TestPowerShellOutputIsUTF8(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("no powershell")
	}
	res, err := NewPowerShellTool().Call(context.Background(), Input{"command": "Write-Output '中文输出'"}, Context{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Data, "中文输出") {
		t.Fatalf("output = %q, want 中文输出", res.Data)
	}
}
