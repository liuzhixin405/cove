package engine

// T3 of docs/superpowers/specs/2026-09-30-test-design.md: a table of tool-call
// batches run through dispatchTools, the classification chain in front of it
// (canonical names, read/write/other grouping, same-file claims, the batch
// checkpoint, the plan-mode gate) and fingerprintToolCalls.
//
// Every case is run in default and plan mode, once with canonical tool names
// and once with the registry aliases (Edit/Write/PowerShell/Agent/ExecutePlan).
// Outlets asserted per run: schedule (which calls overlapped, which ran
// strictly after which), checkpoint count, which calls plan mode refused, the
// results. Invariants checked on every run whatever the case declares: two
// calls writing one file (after path normalization) never overlap; a batch
// with a write/edit gets exactly one checkpoint in default mode and at most
// one in plan mode; the alias run equals the canonical run on every outlet.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/shell"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// ---------------------------------------------------------------------------
// Probe tools
// ---------------------------------------------------------------------------

const (
	// tdSerialWindow is how long a call expected to run alone stays in
	// flight, so a sibling wrongly started beside it is seen overlapping.
	tdSerialWindow = 4 * time.Millisecond
	// tdParallelWait bounds how long a call expected to overlap waits for
	// its group; reached only when the product fails to overlap them.
	tdParallelWait = 400 * time.Millisecond
)

// tdRec records every probe call of one run: start/end sequence numbers and
// which calls were in flight together. A call expected to overlap (target > 1)
// waits until that many calls are in flight, so a correct run is fast and
// deterministic; a call expected to run alone stays in flight for a short
// window so a wrong overlap is caught.
type tdRec struct {
	mu       sync.Mutex
	seq      int
	start    map[string]int
	end      map[string]int
	inflight map[string]bool
	overlap  map[[2]string]bool
	target   map[string]int
	peak     map[string]int // most calls in flight at once while this one was
	changed  chan struct{}
}

func newTDRec(target map[string]int) *tdRec {
	return &tdRec{
		start: map[string]int{}, end: map[string]int{}, inflight: map[string]bool{},
		overlap: map[[2]string]bool{}, target: target, peak: map[string]int{}, changed: make(chan struct{}),
	}
}

func tdPair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func (r *tdRec) signalLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *tdRec) run(id string) {
	r.mu.Lock()
	r.seq++
	r.start[id] = r.seq
	for o := range r.inflight {
		r.overlap[tdPair(id, o)] = true
	}
	r.inflight[id] = true
	for o := range r.inflight {
		r.peak[o] = max(r.peak[o], len(r.inflight))
	}
	r.signalLocked()
	tgt := r.target[id]
	r.mu.Unlock()

	if tgt <= 1 {
		time.Sleep(tdSerialWindow)
	} else {
		deadline := time.After(tdParallelWait)
	wait:
		for {
			r.mu.Lock()
			n, ch := r.peak[id], r.changed
			r.mu.Unlock()
			if n >= tgt {
				break
			}
			select {
			case <-ch:
			case <-deadline:
				break wait
			}
		}
	}

	r.mu.Lock()
	r.seq++
	r.end[id] = r.seq
	delete(r.inflight, id)
	r.signalLocked()
	r.mu.Unlock()
}

// tdProbe is a registrable tool with the flags of a real one. Shells answer
// Allowed only for the lines listed in roLines (as the real shell tools do for
// read-only lines) and ask otherwise.
type tdProbe struct {
	def     tool.Def
	shell   bool
	roLines map[string]bool
	rec     *tdRec
}

func (p *tdProbe) Def() tool.Def {
	d := p.def
	d.InputSchema = json.RawMessage(`{"type":"object"}`)
	d.UserFacingName = d.Name
	return d
}
func (p *tdProbe) Validate(tool.Input) string { return "" }
func (p *tdProbe) CheckPermissions(in tool.Input, _ tool.Context) tool.PermissionDecision {
	if p.shell {
		cmd, _ := in["command"].(string)
		if p.roLines[cmd] {
			return tool.Allowed("read-only line")
		}
		return tool.PermissionDecision{Decision: tool.Ask, Reason: "shell line"}
	}
	return tool.PermissionDecision{Decision: tool.Allow}
}
func (p *tdProbe) Call(_ context.Context, _ tool.Input, tctx tool.Context) (tool.Result, error) {
	p.rec.run(tctx.ToolUseID)
	return tool.Result{Data: "ok " + tctx.ToolUseID}, nil
}

// tdAlias maps canonical names to the registry alias the real tool declares.
var tdAlias = map[string]string{
	"read": "Read", "write": "Write", "edit": "Edit", "powershell": "PowerShell",
	"agent": "Agent", "execute_plan": "ExecutePlan",
}

func tdCanonical(name string) string {
	for c, a := range tdAlias {
		if a == name {
			return c
		}
	}
	return name
}

var tdShellRO = map[string]bool{"ls": true, "git status": true, "Get-ChildItem": true}

// tdTools is the registry of one run, with the flags of the real tools
// (internal/tool): read/grep read-only and concurrency-safe; write/edit/
// bash/powershell neither; agent concurrency-safe and PlanSafe; execute_plan
// PlanSafe only; an MCP tool concurrency-safe, not read-only, not PlanSafe.
func tdTools(rec *tdRec) []tool.Tool {
	mk := func(d tool.Def) *tdProbe {
		if a, ok := tdAlias[d.Name]; ok {
			d.Aliases = []string{a}
		}
		return &tdProbe{def: d, rec: rec}
	}
	shell := func(name string) *tdProbe {
		p := mk(tool.Def{Name: name})
		p.shell, p.roLines = true, tdShellRO
		return p
	}
	return []tool.Tool{
		mk(tool.Def{Name: "read", IsReadOnly: true, IsConcurrencySafe: true}),
		mk(tool.Def{Name: "grep", IsReadOnly: true, IsConcurrencySafe: true}),
		mk(tool.Def{Name: "write"}),
		mk(tool.Def{Name: "edit"}),
		shell("bash"),
		shell("powershell"),
		mk(tool.Def{Name: "agent", IsConcurrencySafe: true, PlanSafe: true}),
		mk(tool.Def{Name: "execute_plan", PlanSafe: true}),
		mk(tool.Def{Name: "mcp__srv__act", IsConcurrencySafe: true}),
	}
}

// ---------------------------------------------------------------------------
// Corpus
// ---------------------------------------------------------------------------

type tdCall struct {
	name string
	args map[string]any // "@abs:" prefix = absolute under the project dir
}

type tdCase struct {
	name  string
	note  string
	calls []tdCall
	// stages is the default-mode schedule of the calls that run: calls in
	// one stage overlap, every call of a stage ends before the next stage's
	// start. Unknown tools never run and are not listed.
	stages [][]int
	cp     bool  // default mode: exactly one checkpoint (false: none)
	deny   []int // calls plan mode refuses
	// skip maps an outlet ("schedule", "checkpoint", "plan", "results",
	// "fingerprint", "alias-equal") to a "bug: ..." note.
	skip map[string]string
}

func c(name string, kv ...string) tdCall {
	args := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		args[kv[i]] = kv[i+1]
	}
	return tdCall{name: name, args: args}
}

var tdPathKeys = []string{"filePath", "file_path", "path", "filepath", "file"}

func tdCorpus() []tdCase {
	cases := []tdCase{
		// read + read
		{name: "read+read/different", calls: []tdCall{c("read", "filePath", "a.go"), c("read", "filePath", "b.go")},
			stages: [][]int{{0, 1}}, note: "readers overlap"},
		{name: "read+read/same", calls: []tdCall{c("read", "filePath", "a.go"), c("read", "file_path", "a.go")},
			stages: [][]int{{0, 1}}, note: "reads of one file still overlap"},
		{name: "grep+read", calls: []tdCall{c("grep", "pattern", "TODO", "path", "."), c("read", "filePath", "a.go")},
			stages: [][]int{{0, 1}}},
		{name: "grep+grep+write", calls: []tdCall{c("grep", "pattern", "x"), c("grep", "pattern", "y"), c("write", "filePath", "a.go")},
			stages: [][]int{{0, 1}, {2}}, cp: true, deny: []int{2}, note: "[write a.go, grep] grepped the old file"},
		{name: "single/read", calls: []tdCall{c("read", "filePath", "a.go")}, stages: [][]int{{0}}},
		{name: "single/write", calls: []tdCall{c("write", "file_path", "a.go")}, stages: [][]int{{0}}, cp: true, deny: []int{0}},

		// read + write, same file
		{name: "read+write/same", calls: []tdCall{c("read", "filePath", "a.go"), c("write", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{1}, note: "[read P, edit P] could read the edited file"},
		{name: "write+read/same", calls: []tdCall{c("write", "filePath", "a.go"), c("read", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0}, note: "[edit P, read P] read P halfway through the edit"},
		{name: "edit+read/abs", calls: []tdCall{c("edit", "file_path", "a.go"), c("read", "filePath", "@abs:a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0}},
		{name: "read+write/different", calls: []tdCall{c("read", "filePath", "a.go"), c("write", "filePath", "b.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{1}, note: "groups never mix kinds"},
		{name: "read+write+read", calls: []tdCall{c("read", "filePath", "a.go"), c("write", "filePath", "b.go"), c("read", "filePath", "c.go")},
			stages: [][]int{{0}, {1}, {2}}, cp: true, deny: []int{1}},

		// write + write, same file
		{name: "write+write/same", calls: []tdCall{c("write", "filePath", "a.go"), c("write", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}, note: "same-file duplicate deferred"},
		{name: "edit+edit/rel-abs", calls: []tdCall{c("edit", "filePath", "a.go"), c("edit", "filePath", "@abs:a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}, note: "internal/x.go and D:\\proj\\internal\\x.go were two claim keys"},
		{name: "write+write/dot-slash", calls: []tdCall{c("write", "filePath", "./a.go"), c("write", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}},
		{name: "write+edit/dotdot", calls: []tdCall{c("write", "filePath", "sub/../a.go"), c("edit", "path", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}},
		{name: "write+write/abs-dot", calls: []tdCall{c("write", "file", "@abs:./a.go"), c("write", "filepath", "@abs:a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}},
		{name: "write,write,write/dup-middle", calls: []tdCall{c("write", "filePath", "a.go"), c("write", "file_path", "@abs:a.go"), c("write", "filePath", "b.go")},
			stages: [][]int{{0, 2}, {1}}, cp: true, deny: []int{0, 1, 2}, note: "the duplicate waits for the group, distinct files still overlap"},
		{name: "edit,edit,edit/dup-last", calls: []tdCall{c("edit", "filePath", "a.go"), c("edit", "filePath", "b.go"), c("edit", "path", "./a.go")},
			stages: [][]int{{0, 1}, {2}}, cp: true, deny: []int{0, 1, 2}},
		{name: "write,write,read/dup-then-reader", calls: []tdCall{c("write", "filePath", "a.go"), c("write", "filePath", "a.go"), c("read", "filePath", "a.go")},
			stages: [][]int{{0}, {1}, {2}}, cp: true, deny: []int{0, 1}, note: "held-back duplicates run before a later reader"},

		// write + write, different files
		{name: "write+write/different", calls: []tdCall{c("write", "filePath", "a.go"), c("write", "filePath", "b.go")},
			stages: [][]int{{0, 1}}, cp: true, deny: []int{0, 1}},
		{name: "write+edit/different-abs", calls: []tdCall{c("write", "filePath", "@abs:a.go"), c("edit", "file_path", "sub/b.go")},
			stages: [][]int{{0, 1}}, cp: true, deny: []int{0, 1}},

		// shells
		{name: "bash-ro+write", calls: []tdCall{c("bash", "command", "ls"), c("write", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{1}, note: "serial call is a barrier; read-only line allowed in plan"},
		{name: "write+bash-ro", calls: []tdCall{c("write", "filePath", "a.go"), c("bash", "command", "git status")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0}},
		{name: "bash-ro+read", calls: []tdCall{c("bash", "command", "ls"), c("read", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, note: "a read-only line takes no checkpoint"},
		{name: "bash-write+read", calls: []tdCall{c("bash", "command", "echo x > a.go"), c("read", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0}, note: "a writing shell line is checkpointed"},
		{name: "read+bash-write", calls: []tdCall{c("read", "filePath", "a.go"), c("bash", "command", "echo x > a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{1}, note: "[read(slow), bash] ran bash during the read"},
		{name: "powershell-ro+edit", calls: []tdCall{c("powershell", "command", "Get-ChildItem"), c("edit", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{1}},
		{name: "bash-write+write/same", calls: []tdCall{c("bash", "command", "echo x > a.go"), c("write", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}, note: "[bash mkdir d, write d/f] wrote before the mkdir"},

		// agent, execute_plan, MCP
		{name: "agent+read", calls: []tdCall{c("agent", "prompt", "refactor a.go"), c("read", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true, note: "[agent refactor P, read P] read P while the agent rewrote it"},
		{name: "read+agent", calls: []tdCall{c("read", "filePath", "a.go"), c("agent", "prompt", "refactor a.go")},
			stages: [][]int{{0}, {1}}, cp: true},
		{name: "agent+agent", calls: []tdCall{c("agent", "prompt", "one"), c("agent", "prompt", "two")},
			stages: [][]int{{0, 1}}, cp: true, note: "agents among themselves overlap; delegation is checkpointed"},
		{name: "edit+agent", calls: []tdCall{c("edit", "filePath", "a.go"), c("agent", "prompt", "refactor a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0}, note: "[edit P, agent refactor P] had two writers"},
		{name: "execute_plan+read", calls: []tdCall{c("execute_plan", "parallel", "true"), c("read", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, cp: true},
		{name: "mcp+read", calls: []tdCall{c("mcp__srv__act", "query", "q"), c("read", "filePath", "a.go")},
			stages: [][]int{{0}, {1}}, deny: []int{0}},
		{name: "read+mcp", calls: []tdCall{c("read", "filePath", "a.go"), c("mcp__srv__act", "query", "q")},
			stages: [][]int{{0}, {1}}, deny: []int{1}},
		{name: "mcp+mcp", calls: []tdCall{c("mcp__srv__act", "query", "q1"), c("mcp__srv__act", "query", "q2")},
			stages: [][]int{{0, 1}}, deny: []int{0, 1}},
		{name: "agent+mcp", calls: []tdCall{c("agent", "prompt", "p"), c("mcp__srv__act", "query", "q")},
			stages: [][]int{{0, 1}}, cp: true, deny: []int{1}, note: "agent and MCP share the 'other concurrency-safe' group"},

		// unknown names
		{name: "unknown+read+read", calls: []tdCall{c("Frobnicate", "filePath", "a.go"), c("read", "filePath", "a.go"), c("read", "filePath", "b.go")},
			stages: [][]int{{1, 2}}},
		{name: "read+unknown+read", calls: []tdCall{c("read", "filePath", "a.go"), c("Frobnicate"), c("read", "filePath", "b.go")},
			stages: [][]int{{0}, {2}}, note: "an unknown tool is a serial barrier"},
		{name: "write+unknown+write/same", calls: []tdCall{c("write", "filePath", "a.go"), c("Frobnicate"), c("write", "filePath", "a.go")},
			stages: [][]int{{0}, {2}}, cp: true, deny: []int{0, 2}},
		{name: "unknown-write-alias", calls: []tdCall{c("WRITE", "filePath", "a.go")},
			note: "not a registered alias: no checkpoint, reported unknown"},

		// aliases written directly in the batch
		{name: "alias/Edit+Write/rel-abs", calls: []tdCall{c("Edit", "file_path", "a.go"), c("Write", "filePath", "@abs:a.go")},
			stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}, note: "checkpointBefore used to see the raw Edit"},
		{name: "alias/Write+edit/different", calls: []tdCall{c("Write", "path", "a.go"), c("edit", "file", "b.go")},
			stages: [][]int{{0, 1}}, cp: true, deny: []int{0, 1}},
	}

	// Every path key on both sides of a same-file and a different-file pair.
	for _, k := range tdPathKeys {
		if k != "filePath" {
			cases = append(cases, tdCase{name: "keys/same/filePath+" + k,
				calls:  []tdCall{c("write", "filePath", "a.go"), c("edit", k, "a.go")},
				stages: [][]int{{0}, {1}}, cp: true, deny: []int{0, 1}, note: "a key alias never claimed its path"})
		}
		cases = append(cases, tdCase{name: "keys/different/" + k,
			calls:  []tdCall{c("edit", k, "a.go"), c("write", k, "b.go")},
			stages: [][]int{{0, 1}}, cp: true, deny: []int{0, 1}})
	}

	// Case-different spellings: one file on Windows, two elsewhere.
	caseStages := [][]int{{0, 1}}
	if runtime.GOOS == "windows" {
		caseStages = [][]int{{0}, {1}}
	}
	cases = append(cases,
		tdCase{name: "write+write/case", calls: []tdCall{c("write", "filePath", "a.go"), c("write", "filePath", "A.GO")},
			stages: caseStages, cp: true, deny: []int{0, 1}, note: "X.go and x.go on Windows ran at once"},
		tdCase{name: "edit+edit/case-abs", calls: []tdCall{c("edit", "file_path", "Sub/A.go"), c("edit", "filePath", "@abs:sub/a.GO")},
			stages: caseStages, cp: true, deny: []int{0, 1}},
	)
	return cases
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// tdEnv is shared by all runs of the test: one project directory and one
// checkpoint manager. Spawning git costs about a second per snapshot on a
// Windows machine with a virus scanner, far over this table's budget, so git
// is kept off PATH: every Create checkpointBefore attempts fails at once and
// is logged as a "[checkpoint]" warning, and the warnings counted during a
// run are the snapshots it took. The real snapshot is covered by
// TestAutoCheckpointCapturesStateBeforeWrites and TestAliasedWriteIsCheckpointed.
type tdEnv struct {
	dir string
	cp  *checkpoint.Manager
}

// tdCPCount receives the "[checkpoint]" warnings of the current run.
var (
	tdCPCount    atomic.Pointer[atomic.Int64]
	tdSinkOnce   sync.Once
	tdSinkPrefix = "[checkpoint] "
)

func tdInstallSink() {
	tdSinkOnce.Do(func() {
		log.AddSink(func(level log.Level, msg string) {
			if n := tdCPCount.Load(); n != nil && level == log.Warn && strings.HasPrefix(msg, tdSinkPrefix) {
				n.Add(1)
			}
		})
	})
}

type tdOutcome struct {
	names    []string // result names
	failed   []bool
	ran      []bool
	refused  []bool // failed with the plan-mode denial
	unknown  []bool
	overlaps map[[2]int]bool
	before   map[[2]int]bool // [a,b]: a ended before b started
	cps      int             // snapshots checkpointBefore attempted
	content  []string
}

func tdResolve(v string, cwd string) string {
	if rest, ok := strings.CutPrefix(v, "@abs:"); ok {
		return filepath.Join(cwd, filepath.FromSlash(rest))
	}
	return v
}

func tdBuild(calls []tdCall, cwd string, rename func(string) string) []api.ToolCall {
	out := make([]api.ToolCall, len(calls))
	for i, cl := range calls {
		in := map[string]any{}
		for k, v := range cl.args {
			if s, ok := v.(string); ok {
				v = tdResolve(s, cwd)
			}
			in[k] = v
		}
		out[i] = api.ToolCall{ID: fmt.Sprintf("c%d", i), Name: rename(cl.name), Input: in}
	}
	return out
}

func tdDesc(calls []api.ToolCall) string {
	parts := make([]string, len(calls))
	for i, tc := range calls {
		keys := make([]string, 0, len(tc.Input))
		for k := range tc.Input {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		kv := make([]string, len(keys))
		for j, k := range keys {
			kv[j] = fmt.Sprintf("%s=%v", k, tc.Input[k])
		}
		parts[i] = tc.Name + "{" + strings.Join(kv, ",") + "}"
	}
	return fmt.Sprintf("%q", strings.Join(parts, "; "))
}

func tdContains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// tdPlanStages is the default schedule without the calls plan mode refuses:
// grouping happens before the gate, so refused calls leave their group's
// boundaries in place.
func tdPlanStages(stages [][]int, deny []int) [][]int {
	var out [][]int
	for _, st := range stages {
		var kept []int
		for _, i := range st {
			if !tdContains(deny, i) {
				kept = append(kept, i)
			}
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

func (env *tdEnv) run(t *testing.T, calls []api.ToolCall, mode permission.Mode, stages [][]int) tdOutcome {
	t.Helper()
	target := map[string]int{}
	for _, st := range stages {
		for _, i := range st {
			target[fmt.Sprintf("c%d", i)] = len(st)
		}
	}
	rec := newTDRec(target)
	eng := newTestEngine(&mockProvider{}, tdTools(rec)...)
	eng.SetProjectContext(&ctxt.ProjectContext{Cwd: env.dir})
	eng.SetPermissionMode(mode)
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return true }

	eng.cpMgr = env.cp
	var cps atomic.Int64
	tdCPCount.Store(&cps)
	results := eng.dispatchTools(context.Background(), calls)
	tdCPCount.Store(nil)

	o := tdOutcome{overlaps: map[[2]int]bool{}, before: map[[2]int]bool{}, cps: int(cps.Load())}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, r := range results {
		o.names = append(o.names, r.Name)
		o.failed = append(o.failed, r.Failed)
		o.content = append(o.content, r.Content)
		_, ran := rec.start[fmt.Sprintf("c%d", i)]
		o.ran = append(o.ran, ran)
		o.refused = append(o.refused, r.Failed && strings.Contains(r.Content, "plan mode"))
		o.unknown = append(o.unknown, r.Failed && strings.Contains(r.Content, "unknown tool"))
	}
	for i := range calls {
		for j := range calls {
			a, b := fmt.Sprintf("c%d", i), fmt.Sprintf("c%d", j)
			if i < j && rec.overlap[tdPair(a, b)] {
				o.overlaps[[2]int{i, j}] = true
			}
			ea, okA := rec.end[a]
			sb, okB := rec.start[b]
			if i != j && okA && okB && ea < sb {
				o.before[[2]int{i, j}] = true
			}
		}
	}
	return o
}

// tdClaim is the test's own normalization of a write/edit target: absolute
// against the project, cleaned, case-folded on Windows.
func tdClaim(tc api.ToolCall, cwd string) string {
	if n := tdCanonical(tc.Name); n != "write" && n != "edit" {
		return ""
	}
	for _, k := range tdPathKeys {
		if v, ok := tc.Input[k].(string); ok && v != "" {
			if !filepath.IsAbs(v) {
				v = filepath.Join(cwd, v)
			}
			v = filepath.Clean(v)
			if runtime.GOOS == "windows" {
				v = strings.ToLower(v)
			}
			return v
		}
	}
	return ""
}

func TestTableDispatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// A store that looks initialized, so checkpoint.New runs no git init.
	store := filepath.Join(home, ".cove", "checkpoints", "store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// shell.Default is resolved once per process from PATH; resolve it now,
	// or an empty PATH would make it cmd for this and every later test
	// (and "ls" would no longer count as read-only).
	_ = shell.Default()
	t.Setenv("PATH", t.TempDir()) // no git: Create fails fast (see tdEnv)
	env := &tdEnv{dir: t.TempDir()}
	t.Chdir(env.dir)
	cp, err := checkpoint.New(env.dir)
	if err != nil {
		t.Fatalf("checkpoint manager: %v", err)
	}
	env.cp = cp
	tdInstallSink()

	for _, tc := range tdCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			env.runCase(t, tc)
		})
	}
}

func (env *tdEnv) runCase(t *testing.T, tc tdCase) {
	canon := tdBuild(tc.calls, env.dir, tdCanonical)
	alias := tdBuild(tc.calls, env.dir, func(n string) string {
		if a, ok := tdAlias[tdCanonical(n)]; ok {
			return a
		}
		return n
	})
	hasAlias := tdDesc(canon) != tdDesc(alias)

	check := func(t *testing.T, outlet string, fn func(t *testing.T)) {
		t.Run(outlet, func(t *testing.T) {
			if s := tc.skip[outlet]; s != "" {
				t.Skip(s)
			}
			fn(t)
		})
	}

	for _, mode := range []permission.Mode{permission.Default, permission.Plan} {
		t.Run(string(mode), func(t *testing.T) {
			stages := tc.stages
			if mode == permission.Plan {
				stages = tdPlanStages(tc.stages, tc.deny)
			}
			got := env.run(t, canon, mode, stages)
			batch := tdDesc(canon)
			env.checkRun(t, check, tc, canon, batch, mode, stages, got)

			if !hasAlias {
				return
			}
			gotA := env.run(t, alias, mode, stages)
			batchA := tdDesc(alias)
			env.checkRun(t, check, tc, alias, batchA, mode, stages, gotA)
			check(t, "alias-equal", func(t *testing.T) {
				cmp := func(what string, a, b any) {
					if fmt.Sprint(a) != fmt.Sprint(b) {
						t.Errorf("batch %s vs %s mode=%s outlet=alias/%s: alias got %v, canonical got %v", batchA, batch, mode, what, b, a)
					}
				}
				cmp("names", got.names, gotA.names)
				cmp("failed", got.failed, gotA.failed)
				cmp("ran", got.ran, gotA.ran)
				cmp("refused", got.refused, gotA.refused)
				cmp("overlaps", tdSortedPairs(got.overlaps), tdSortedPairs(gotA.overlaps))
				cmp("before", tdSortedPairs(got.before), tdSortedPairs(gotA.before))
				cmp("checkpoints", got.cps, gotA.cps)
			})
		})
	}

	check(t, "fingerprint", func(t *testing.T) {
		eng := &Engine{}
		batch := tdDesc(canon)
		fp := eng.fingerprintToolCalls(canon)
		// Changing any call's target file changes the fingerprint.
		for i, call := range canon {
			key := tdFileKey(call)
			if key == "" {
				continue
			}
			mut := tdBuild(tc.calls, env.dir, tdCanonical)
			mut[i].Input[key] = fmt.Sprintf("zz_other_%d.go", i)
			if fpm := eng.fingerprintToolCalls(mut); fpm == fp {
				t.Errorf("batch %s outlet=fingerprint: retargeting call %d (%s) to another file kept fingerprint %q; want a different one", batch, i, key, fp)
			}
		}
		// Two calls to different files are two fingerprint parts.
		for i := range canon {
			for j := i + 1; j < len(canon); j++ {
				a, b := tdClaim(canon[i], env.dir), tdClaim(canon[j], env.dir)
				if a == "" || b == "" || a == b || tdCanonical(canon[i].Name) != tdCanonical(canon[j].Name) {
					continue
				}
				pi := eng.fingerprintToolCalls(canon[i : i+1])
				pj := eng.fingerprintToolCalls(canon[j : j+1])
				if pi == pj {
					t.Errorf("batch %s outlet=fingerprint: calls %d and %d target different files but fingerprint alike: %q", batch, i, j, pi)
				}
			}
		}
	})
	if hasAlias {
		check(t, "alias-fingerprint", func(t *testing.T) {
			// The fingerprint keys on the registry's canonical name, so the
			// engine needs the registry: with a bare Engine every alias
			// fingerprinted as itself and the loop detector kept two
			// histories for `Edit` and `edit` on one file.
			eng := newTestEngine(&mockProvider{}, tdTools(newTDRec(map[string]int{}))...)
			fc, fa := eng.fingerprintToolCalls(canon), eng.fingerprintToolCalls(alias)
			if fc != fa {
				t.Errorf("batch %s outlet=fingerprint: alias got %q, canonical %q, want equal", tdDesc(alias), fa, fc)
			}
		})
	}
}

// tdFileKey is the key naming the call's target file, as toolKeyArg reads
// it: every file key for read/write/edit, "path" excluded for other tools.
func tdFileKey(tc api.ToolCall) string {
	n := tdCanonical(tc.Name)
	for _, k := range tdPathKeys {
		if k == "path" && n != "read" && n != "write" && n != "edit" {
			continue
		}
		if v, ok := tc.Input[k].(string); ok && v != "" {
			return k
		}
	}
	return ""
}

func tdSortedPairs(m map[[2]int]bool) []string {
	var out []string
	for p := range m {
		out = append(out, fmt.Sprintf("%d-%d", p[0], p[1]))
	}
	sort.Strings(out)
	return out
}

func (env *tdEnv) checkRun(t *testing.T, check func(*testing.T, string, func(*testing.T)), tc tdCase,
	calls []api.ToolCall, batch string, mode permission.Mode, stages [][]int, got tdOutcome) {
	t.Helper()
	isPlan := mode == permission.Plan
	label := "canonical"
	if tdDesc(tdBuild(tc.calls, env.dir, tdCanonical)) != batch {
		label = "alias"
	}

	t.Run(label, func(t *testing.T) {
		check(t, "results", func(t *testing.T) {
			for i, call := range calls {
				wantName := tdCanonical(call.Name)
				if got.names[i] != wantName {
					t.Errorf("batch %s mode=%s outlet=results call %d: name got %q want %q", batch, mode, i, got.names[i], wantName)
				}
				registered := tdCanonical(call.Name) != call.Name || tdIsRegistered(call.Name)
				switch {
				case !registered:
					if !got.unknown[i] || got.ran[i] {
						t.Errorf("batch %s mode=%s outlet=results call %d: unknown tool got failed=%v ran=%v %q, want an unknown-tool error", batch, mode, i, got.failed[i], got.ran[i], got.content[i])
					}
				case isPlan && tdContains(tc.deny, i):
					// asserted by the plan outlet
				default:
					if got.failed[i] || !got.ran[i] {
						t.Errorf("batch %s mode=%s outlet=results call %d: got failed=%v ran=%v %q, want it to run", batch, mode, i, got.failed[i], got.ran[i], got.content[i])
					}
				}
			}
		})

		check(t, "plan", func(t *testing.T) {
			for i := range calls {
				want := isPlan && tdContains(tc.deny, i)
				if got.refused[i] != want || (want && got.ran[i]) {
					t.Errorf("batch %s mode=%s outlet=plan call %d: refused got %v (ran=%v, %q), want %v", batch, mode, i, got.refused[i], got.ran[i], got.content[i], want)
				}
			}
		})

		check(t, "schedule", func(t *testing.T) {
			for si, st := range stages {
				for x := 0; x < len(st); x++ {
					for y := x + 1; y < len(st); y++ {
						a, b := st[x], st[y]
						if a > b {
							a, b = b, a
						}
						if !got.overlaps[[2]int{a, b}] {
							t.Errorf("batch %s mode=%s outlet=schedule: calls %d and %d got sequential, want parallel (stages %v)", batch, mode, a, b, stages)
						}
					}
				}
				for _, later := range stages[si+1:] {
					for _, a := range st {
						for _, b := range later {
							if !got.before[[2]int{a, b}] {
								t.Errorf("batch %s mode=%s outlet=schedule: call %d got not finished before call %d started, want sequential (stages %v; overlaps %v)", batch, mode, a, b, stages, tdSortedPairs(got.overlaps))
							}
						}
					}
				}
			}
		})

		// Invariant: two calls writing one file never overlap.
		check(t, "inv-same-file", func(t *testing.T) {
			for i := range calls {
				for j := i + 1; j < len(calls); j++ {
					a, b := tdClaim(calls[i], env.dir), tdClaim(calls[j], env.dir)
					if a != "" && a == b && got.overlaps[[2]int{i, j}] {
						t.Errorf("batch %s mode=%s outlet=inv-same-file: calls %d and %d both write %s and got parallel, want sequential", batch, mode, i, j, a)
					}
				}
			}
		})

		check(t, "checkpoint", func(t *testing.T) {
			hasWrite := false
			for _, call := range calls {
				if n := tdCanonical(call.Name); n == "write" || n == "edit" {
					hasWrite = true
				}
			}
			if isPlan {
				// Invariant: never more than one per batch. Whether a batch
				// plan mode refuses entirely should take none is 待确认: today
				// checkpointBefore runs before the gate and takes one.
				if got.cps > 1 {
					t.Errorf("batch %s mode=%s outlet=checkpoint: got %d checkpoints, want at most 1", batch, mode, got.cps)
				}
				return
			}
			want := 0
			if tc.cp {
				want = 1
			}
			if got.cps != want {
				t.Errorf("batch %s mode=%s outlet=checkpoint: got %d checkpoints, want %d", batch, mode, got.cps, want)
			}
			if hasWrite && got.cps != 1 {
				t.Errorf("batch %s mode=%s outlet=inv-checkpoint: batch has a write/edit, got %d checkpoints, want exactly 1", batch, mode, got.cps)
			}
		})
	})
}

var tdRegistered = func() map[string]bool {
	m := map[string]bool{}
	for _, tl := range tdTools(nil) {
		m[tl.Def().Name] = true
	}
	return m
}()

func tdIsRegistered(name string) bool { return tdRegistered[name] }
