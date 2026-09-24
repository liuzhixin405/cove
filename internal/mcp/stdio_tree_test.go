package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperSpawnTree is the "tree" helper mode: it behaves like a launcher
// (npx.cmd -> cmd.exe -> node, uvx -> python) that starts the real server as
// a grandchild and waits on it. It prints the grandchild's pid, then ignores
// stdin like a wrapper that is itself waiting for its child.
func helperSpawnTree() {
	gc := exec.Command(os.Args[0], helperArgs()...)
	gc.Env = append(os.Environ(), helperEnvFlag+"=1", helperEnvMode+"=ignore-stdin")
	if err := gc.Start(); err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println(gc.Process.Pid)
	time.Sleep(60 * time.Second)
}

// TestStdioTransport_CloseKillsTheWholeProcessTree: a server launched through
// a wrapper leaves the real server as a grandchild. Kill() only reached the
// wrapper, so the grandchild was orphaned and kept running (holding ports,
// files and memory) long after cove had "disconnected" it or exited.
func TestStdioTransport_CloseKillsTheWholeProcessTree(t *testing.T) {
	tr := newHelperTransport(t, "tree")

	line, err := tr.reader.ReadString('\n')
	if err != nil {
		tr.Close()
		t.Fatalf("reading grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		tr.Close()
		t.Fatalf("helper printed %q; want a pid", line)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	if !processAlive(pid) {
		t.Fatal("grandchild is not running before Close")
	}

	_ = tr.Close()

	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still running after Close: the server tree was orphaned", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
