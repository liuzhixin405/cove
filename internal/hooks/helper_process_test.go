package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

// The command-hook tests need a real external executable. Rather than depend on
// a shell or any binary being installed, they re-execute the test binary
// itself, which is the standard helper-process pattern.
//
// The usual form of that pattern passes `-test.run=TestHelperProcess` as an
// argument, but HookConfig.Command is a bare path: Manager.runCommand builds
// exec.CommandContext(ctx, cmdPath) with NO arguments, so there is nowhere to
// put the flag. Instead, the mode is passed through the environment (which the
// child inherits, because runCommand leaves cmd.Env nil) and TestMain
// dispatches on it before testing.M.Run is ever reached.
const (
	helperModeEnv = "COVE_HOOK_HELPER_MODE"
	helperAddrEnv = "COVE_HOOK_HELPER_ADDR"
)

// helper modes.
const (
	// modeEchoInput parses the HookInput from stdin and reports what it saw in
	// the Message, proving the input reached the child as JSON.
	modeEchoInput = "echo-input"
	// modeBlock returns a JSON HookOutput with Continue=false.
	modeBlock = "block"
	// modeNonJSON writes plain text to stdout.
	modeNonJSON = "non-json"
	// modeFail exits non-zero without output.
	modeFail = "fail"
	// modeDialExit connects back to the test's listener, announces itself and
	// exits immediately.
	modeDialExit = "dial-exit"
	// modeDialHang connects back, announces itself and then blocks forever, so
	// the test can observe whether the hook's timeout kills it.
	modeDialHang = "dial-hang"
	// modeJSONNoContinue prints a JSON object that carries a message but no
	// "continue" field, the way a hook that only wants to add a note would.
	modeJSONNoContinue = "json-no-continue"
	// modeSpawnBackground starts a long-lived grandchild that inherits the
	// hook's stdout, prints a normal result and exits: a hook that launches a
	// notifier or daemon in the background.
	modeSpawnBackground = "spawn-background"
	// modeDialWaitClose connects back, announces itself and stays alive until
	// the test closes the connection. It is the grandchild of
	// modeSpawnBackground, so the test can clean it up deterministically.
	modeDialWaitClose = "dial-wait-close"
)

const helperMarker = "hook-ran"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		runHelperProcess(mode)
		return
	}
	// Hook errors and regex errors go through log.Warnf; keep them off the
	// test output. This only affects this package's test binary.
	log.SetWriter(io.Discard)
	os.Exit(m.Run())
}

// runHelperProcess is the child-process entry point. It must never call
// testing.M.Run, and must write nothing to stdout beyond what the mode says,
// since the parent parses stdout as the HookOutput.
func runHelperProcess(mode string) {
	switch mode {
	case modeEchoInput:
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: read stdin: %v\n", err)
			os.Exit(1)
		}
		var in HookInput
		if err := json.Unmarshal(raw, &in); err != nil {
			// Report the failure as a parseable output so the parent sees a
			// precise assertion failure instead of a generic exec error.
			emit(HookOutput{Continue: true, Message: fmt.Sprintf("stdin-not-json: %v (%q)", err, raw)})
			os.Exit(0)
		}
		emit(HookOutput{
			Continue: true,
			Modified: true,
			Message: fmt.Sprintf("event=%s tool=%s model=%s path=%v messages=%d",
				in.Event, in.ToolName, in.Model, in.ToolInput["path"], len(in.Messages)),
		})
	case modeBlock:
		_, _ = io.ReadAll(os.Stdin)
		emit(HookOutput{Continue: false, Message: "denied by command hook"})
	case modeNonJSON:
		_, _ = io.ReadAll(os.Stdin)
		fmt.Print("not json at all")
	case modeFail:
		_, _ = io.ReadAll(os.Stdin)
		fmt.Fprintln(os.Stderr, "helper: failing on purpose")
		os.Exit(3)
	case modeDialExit:
		announce()
	case modeDialHang:
		announce()
		// A pending timer keeps the runtime's deadlock detector quiet; the
		// process is expected to be killed by the hook's timeout.
		time.Sleep(10 * time.Minute)
	case modeJSONNoContinue:
		_, _ = io.ReadAll(os.Stdin)
		fmt.Print(`{"message":"note from hook"}`)
	case modeSpawnBackground:
		_, _ = io.ReadAll(os.Stdin)
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: os.Executable: %v\n", err)
			os.Exit(7)
		}
		child := exec.Command(exe)
		child.Env = append(os.Environ(), helperModeEnv+"="+modeDialWaitClose)
		child.Stdout = os.Stdout // the grandchild keeps the hook's stdout open
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "helper: start grandchild: %v\n", err)
			os.Exit(8)
		}
		emit(HookOutput{Continue: true, Message: "spawned"})
	case modeDialWaitClose:
		conn := announceConn()
		_, _ = io.Copy(io.Discard, conn) // returns when the test closes it
	default:
		fmt.Fprintf(os.Stderr, "helper: unknown mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

// announce connects to the address the test is listening on and writes a
// marker. Accepting that connection is the test's proof that the hook process
// actually started, with no sleeping and no polling.
func announce() {
	_ = announceConn()
}

// announceConn is announce, returning the connection for modes that need to
// watch it.
func announceConn() net.Conn {
	addr := os.Getenv(helperAddrEnv)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: dial %q: %v\n", addr, err)
		os.Exit(4)
	}
	if _, err := io.WriteString(conn, helperMarker); err != nil {
		fmt.Fprintf(os.Stderr, "helper: write: %v\n", err)
		os.Exit(5)
	}
	// Deliberately left open: the parent detects the process dying by the
	// connection closing.
	return conn
}

func emit(out HookOutput) {
	blob, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: marshal: %v\n", err)
		os.Exit(6)
	}
	os.Stdout.Write(blob)
}
