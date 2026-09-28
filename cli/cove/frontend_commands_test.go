package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/tool"
)

// headlessFrontend is the headless front end's command dispatch with its
// notices captured.
func headlessFrontend(t *testing.T) (*frontend, *[]string) {
	t.Helper()
	var notices []string
	fe := &frontend{eng: newTestEngine(t), cfg: config.DefaultConfig(), toolReg: tool.NewRegistry(),
		print:   func(s string) { notices = append(notices, s) },
		enqueue: func(api.Message) {},
	}
	fe.install(registerAllCommands())
	return fe, &notices
}

// Headless answered "未找到命令" for /continue and /clear, and bare /model
// and /mode were unknown in both front ends (only "/model x" was matched).
func TestHeadlessKnowsTheFrontEndCommands(t *testing.T) {
	out := captureOut(t)
	fe, notices := headlessFrontend(t)
	for _, in := range []string{"/continue", "/clear", "/model", "/mode", "/tasks", "/stop"} {
		*notices = nil
		out.Reset()
		if !fe.dispatch(in) {
			t.Fatalf("%s not dispatched", in)
		}
		all := strings.Join(*notices, "\n") + out.String()
		if strings.Contains(all, "未找到命令") || strings.Contains(all, "Unknown") {
			t.Errorf("%s is unknown in headless: %s", in, all)
		}
		if strings.TrimSpace(all) == "" {
			t.Errorf("%s said nothing", in)
		}
	}
}

// One list: every registered command is offered by completion and listed
// by /help.
func TestCompletionAndHelpComeFromTheRegistry(t *testing.T) {
	reg := (&frontend{}).install(registerAllCommands())
	entries := map[string]cmdEntry{}
	for _, e := range buildCommandList(reg, tool.NewRegistry()) {
		entries[e.Name] = e
	}
	out := captureOut(t)
	printHelp(reg, tool.NewRegistry(), nil)
	help := out.String()
	for _, c := range reg.All() {
		if _, ok := entries["/"+c.Name()]; !ok {
			t.Errorf("/%s is registered but not offered by completion", c.Name())
		}
		if !strings.Contains(help, "/"+c.Name()+" ") && !strings.Contains(help, "/"+c.Name()+"\n") {
			t.Errorf("/%s is registered but not in /help", c.Name())
		}
	}
	if hints := entries["/mode"].ArgHints[""]; len(hints) != 4 {
		t.Errorf("/mode completion hints = %v", hints)
	}
}

// A front-end command registered over a generic one replaces it: one entry
// per name, and the generic command still handles what the front-end one
// does not (/config <key> <value>).
func TestFrontEndCommandsReplaceGenericOnes(t *testing.T) {
	reg := (&frontend{}).install(registerAllCommands())
	seen := map[string]int{}
	for _, c := range reg.All() {
		seen[c.Name()]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("/%s listed %d times", name, n)
		}
	}
	c, ok := reg.Find("config")
	if !ok {
		t.Fatal("/config not registered")
	}
	if fc, ok := c.(*feCmd); !ok || fc.base == nil {
		t.Fatalf("/config is not the front-end command over the generic one: %T", c)
	}
}
