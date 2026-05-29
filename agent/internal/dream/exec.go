package dream

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// executeReadOnlyCommand runs a shell command with a timeout, returning stdout+stderr.
func executeReadOnlyCommand(cmd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}

	output, err := c.CombinedOutput()
	result := strings.TrimSpace(string(output))

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "Error: command timed out (30s)"
		}
		if result != "" {
			return result
		}
		return "Error: " + err.Error()
	}

	// Truncate large outputs
	if len(result) > 20000 {
		result = result[:20000] + "\n... [truncated]"
	}
	return result
}
