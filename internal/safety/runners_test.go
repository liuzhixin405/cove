package safety

import (
	"reflect"
	"strings"
	"testing"
)

func TestStripCommandRunners(t *testing.T) {
	cases := map[string]string{
		"git push":                    "git push",
		"sudo git push":               "git push",
		"sudo -u root -E git push":    "git push",
		"/usr/bin/sudo git push":      "git push",
		"env X=1 git push":            "git push",
		"env -i -u HOME X=1 git push": "git push",
		"X=1 Y=2 git push":            "git push",
		"command -p git push":         "git push",
		"nohup nice -n 5 git push":    "git push",
		"timeout -s KILL 10 git push": "git push",
		"time -p busybox rm -rf x":    "rm -rf x",
		"exec -a name git push":       "git push",
		"sudo":                        "",
		"env -- X=1 --weird git push": "--weird git push",
	}
	for in, want := range cases {
		got := strings.Join(StripCommandRunners(strings.Fields(in)), " ")
		if got != want {
			t.Errorf("StripCommandRunners(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ProgramName(`C:\Git\bin\GIT.EXE`); got != "git" {
		t.Errorf("ProgramName = %q", got)
	}
	if name, args := commandWords(strings.Fields("sudo /bin/RM -rf /")); name != "rm" || !reflect.DeepEqual(args, []string{"-rf", "/"}) {
		t.Errorf("commandWords = %q %v", name, args)
	}
}
