package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// progressWriter forwards everything written to it into a buffer and (if set)
// to an onProgress callback, so callers get both the full captured output and a
// live stream of chunks. os/exec invokes Write from a dedicated copy goroutine.
type progressWriter struct {
	buf        *bytes.Buffer
	onProgress func(chunk string)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if w.onProgress != nil {
		w.onProgress(string(p))
	}
	return len(p), nil
}

// streamCommand runs cmd, streaming its stdout/stderr live into the provided
// buffers (and through onProgress) while remaining cancellable.
//
// It returns the process exit code and any non-exit error. A non-zero exit
// status is reported via exitCode with runErr==nil (mirroring the previous
// cmd.Run() handling). A timeout or cancellation is reported via runErr.
//
// Robustness against hangs: a command may spawn children (servers, build
// daemons, etc.) that inherit the output pipes. The default ctx-cancel only
// kills the top process, leaving those children alive and the pipes open, which
// would block reads forever. We defend against this two ways:
//   - Cancel kills the whole process tree (taskkill /T on Windows, the process
//     group on Unix) so inherited pipes close promptly.
//   - WaitDelay is a backstop: if I/O is still pending shortly after the process
//     exits, os/exec force-closes the pipes so Run() returns instead of hanging.
func streamCommand(ctx context.Context, cmd *exec.Cmd, stdout, stderr *bytes.Buffer, onProgress func(chunk string)) (exitCode int, runErr error) {
	cmd.Stdout = &progressWriter{buf: stdout, onProgress: onProgress}
	cmd.Stderr = &progressWriter{buf: stderr, onProgress: onProgress}

	// Put the child in its own process group (Unix) so the whole tree can be
	// signalled at once. No-op on Windows, where taskkill /T handles the tree.
	setupProcessGroup(cmd)

	// Override the default ctx-cancel (which only kills the lead process) with a
	// process-tree kill so children releasing the pipes can't keep us hanging.
	if ctx != nil && ctx.Done() != nil {
		cmd.Cancel = func() error { return killProcessTree(cmd) }
	}
	// Backstop: even if the tree kill misses something, force-close the pipes a
	// few seconds after the process exits so Run() can't block indefinitely.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()

	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return 0, fmt.Errorf("命令运行超时，已被强制终止")
			}
			return 0, fmt.Errorf("命令已被取消")
		}
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 0, err
	}
	return 0, nil
}
