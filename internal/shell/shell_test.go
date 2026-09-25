package shell

import (
	"errors"
	"strings"
	"testing"
)

// fakeHost describes a machine: which names PATH resolves and which files exist.
type fakeHost struct {
	goos  string
	path  map[string]string
	files map[string]bool
	env   map[string]string
}

func (h fakeHost) probe() probe {
	return probe{
		goos: h.goos,
		lookPath: func(name string) (string, error) {
			if p, ok := h.path[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		exists: func(p string) bool { return h.files[strings.ToLower(p)] },
		getenv: func(k string) string { return h.env[k] },
	}
}

func TestDetectPrefersGitBashOverWSLLauncher(t *testing.T) {
	// The default Windows PATH lists System32 before Git\cmd, so "bash"
	// resolves to the WSL launcher. With no distro installed every command
	// fails with "execvpe(/bin/bash) failed".
	host := fakeHost{
		goos: "windows",
		path: map[string]string{
			"bash": `C:\Windows\system32\bash.exe`,
			"git":  `D:\Program Files\Git\cmd\git.exe`,
			"pwsh": `C:\Program Files\PowerShell\7\pwsh.exe`,
		},
		files: map[string]bool{strings.ToLower(`D:\Program Files\Git\bin\bash.exe`): true},
	}
	got := detect(host.probe())
	if got.Path != `D:\Program Files\Git\bin\bash.exe` || got.Kind != Bash {
		t.Fatalf("detect = %+v, want Git Bash", got)
	}
}

func TestDetectFindsGitBashFromMingwGit(t *testing.T) {
	host := fakeHost{
		goos:  "windows",
		path:  map[string]string{"git": `C:\Git\mingw64\bin\git.exe`},
		files: map[string]bool{strings.ToLower(`C:\Git\bin\bash.exe`): true},
	}
	if got := detect(host.probe()); got.Path != `C:\Git\bin\bash.exe` {
		t.Fatalf("detect = %+v, want C:\\Git\\bin\\bash.exe", got)
	}
}

func TestDetectSkipsWindowsAppsLauncher(t *testing.T) {
	host := fakeHost{
		goos: "windows",
		path: map[string]string{
			"bash":       `C:\Users\u\AppData\Local\Microsoft\WindowsApps\bash.exe`,
			"powershell": `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		},
	}
	got := detect(host.probe())
	if got.Kind != PowerShell {
		t.Fatalf("detect = %+v, want PowerShell fallback", got)
	}
}

func TestDetectUsesRealBashOnPath(t *testing.T) {
	host := fakeHost{
		goos: "windows",
		path: map[string]string{"bash": `C:\msys64\usr\bin\bash.exe`},
	}
	if got := detect(host.probe()); got.Path != `C:\msys64\usr\bin\bash.exe` {
		t.Fatalf("detect = %+v, want msys bash", got)
	}
}

func TestDetectFallsBackToCmd(t *testing.T) {
	host := fakeHost{goos: "windows", path: map[string]string{}}
	if got := detect(host.probe()); got.Kind != Cmd {
		t.Fatalf("detect = %+v, want cmd", got)
	}
}

func TestDetectUnixUsesBash(t *testing.T) {
	host := fakeHost{goos: "linux", path: map[string]string{"bash": "/usr/bin/bash"}}
	got := detect(host.probe())
	if got.Kind != Bash || got.Path != "/usr/bin/bash" {
		t.Fatalf("detect = %+v, want /usr/bin/bash", got)
	}
}

func TestCommandArgs(t *testing.T) {
	cases := []struct {
		sh   Shell
		want []string
	}{
		{Shell{Kind: Bash, Path: "bash"}, []string{"-c", "echo hi"}},
		{Shell{Kind: PowerShell, Path: "pwsh"}, []string{"-NoProfile", "-NonInteractive", "-Command", psUTF8 + "echo hi"}},
		// cmd writes in the console code page unless switched to UTF-8.
		{Shell{Kind: Cmd, Path: "cmd"}, []string{"/C", "chcp 65001>nul & echo hi"}},
	}
	for _, c := range cases {
		if got := c.sh.Args("echo hi"); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%v.Args = %q, want %q", c.sh.Kind, got, c.want)
		}
	}
}
