package permission

// Corpus runner for the permission classifier (test design spec §4.2, T1).
//
// Every corpus file (corpus_<dimension>_test.go) contributes a []permCase.
// runCorpus runs each case under every applicable shell kind, asserts the
// six outlets against the case's expectation and checks the cross-outlet
// invariants, which need no per-case declaration.
//
// The six outlets:
//
//	readOnly     Classifier.IsReadOnlyLineFor(cmd, kind)
//	auto         Classifier.AutoApproveLineFor(cmd, kind)
//	cat          Classifier.ClassifyLineFor(cmd, kind)
//	remember     ShellRememberRules("bash", cmd, kind), reduced to rememberKind
//	catastrophic safety.CatastrophicCommand(cmd)
//	check        the decision for a bash call under each scenario (see decide)

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/safety"
)

// rememberKind is what an "[a]/[p]" answer to a line would remember.
type rememberKind int

const (
	rememberNone   rememberKind = iota // no [a]/[p] offered
	rememberPrefix                     // only CommandPrefix rules
	rememberGroup                      // only CommandGroup rules
	rememberMixed                      // both prefix and group rules
)

func (r rememberKind) String() string {
	switch r {
	case rememberPrefix:
		return "prefix"
	case rememberGroup:
		return "group"
	case rememberMixed:
		return "prefix+group"
	}
	return "none"
}

// permCase is one corpus entry. kinds nil means the case applies to all
// three shell kinds with the same expectation (invariant 5 then also
// requires the three kinds to agree on every outlet).
type permCase struct {
	cmd   string
	kinds []ShellKind
	want  permWant
	note  string // the bug class the case guards
	// skip, when set, marks a known product bug ("bug: ..."): the case is
	// kept with its correct expectation and skipped until the bug is fixed.
	skip string
}

// permWant is the expectation for the six outlets. Unset fields are the
// zero value: "must be false / none". check lists only the scenarios the
// case cares about.
type permWant struct {
	readOnly, auto bool
	cat            CmdCategory
	remember       rememberKind
	catastrophic   bool
	check          map[string]Decision
}

// permGot is what the six outlets returned for one command under one kind.
type permGot struct {
	readOnly, auto bool
	cat            CmdCategory
	remember       rememberKind
	catastrophic   bool
	check          map[string]Decision
}

var allKinds = []ShellKind{ShellPOSIX, ShellPowerShell, ShellCmd}

// Shorthands for the kinds field.
var (
	kPOSIX   = []ShellKind{ShellPOSIX}
	kPS      = []ShellKind{ShellPowerShell}
	kCmd     = []ShellKind{ShellCmd}
	kPosixPS = []ShellKind{ShellPOSIX, ShellPowerShell}
	kPosixCm = []ShellKind{ShellPOSIX, ShellCmd}
)

// Scenario names (spec §4.2: rule set × mode).
const (
	scDefault    = "default"
	scAuto       = "auto"
	scPlan       = "plan"
	scBypass     = "bypass"
	scAllowGo    = "allow-prefix-go-test"
	scDenyPush   = "deny-git-push"
	scAskGit     = "ask-git-group"
	scParamMatch = "param-match-git-status"
)

type scenario struct {
	name  string
	mode  Mode
	rules func(m *Manager)
}

var scenarios = []scenario{
	{scDefault, Default, func(*Manager) {}},
	{scAuto, Auto, func(*Manager) {}},
	{scPlan, Plan, func(*Manager) {}},
	{scBypass, Bypass, func(*Manager) {}},
	{scAllowGo, Default, func(m *Manager) {
		m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "go test"})
	}},
	{scDenyPush, Default, func(m *Manager) {
		m.AddRule(DAllow, Rule{ToolPattern: "bash"})
		m.AddRule(DDeny, Rule{ToolPattern: "bash", CommandPrefix: "git push"})
	}},
	{scAskGit, Auto, func(m *Manager) {
		m.AddRule(DAsk, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})
	}},
	{scParamMatch, Default, func(m *Manager) {
		r, ok := PolicyRule{ID: "allow-git-status", ToolPattern: "bash", Action: ActionAllow, Enabled: true,
			ParamMatch: map[string]string{"command": "git status*"}}.ToRule()
		if !ok {
			panic("param_match policy rule did not convert")
		}
		m.AddRule(DAllow, r)
	}},
}

var (
	managersOnce sync.Once
	managers     map[string]map[ShellKind]*Manager
	corpusCls    = NewClassifier()
)

// scenarioManager returns the shared Manager of a scenario under a shell
// kind. Managers are only read (Check) after construction.
func scenarioManager(name string, kind ShellKind) *Manager {
	managersOnce.Do(func() {
		managers = map[string]map[ShellKind]*Manager{}
		for _, s := range scenarios {
			managers[s.name] = map[ShellKind]*Manager{}
			for _, k := range allKinds {
				m := NewManager(s.mode)
				m.SetShellKind(k)
				m.SetBypassAvailable(true)
				s.rules(m)
				managers[s.name][k] = m
			}
		}
	})
	return managers[name][kind]
}

// decide is the decision for a bash call running cmd under kind in scenario
// s, following the engine's chain (engine.executeTool / authorizeToolCall)
// with the mode tiers the manual's 权限模式 table documents:
//
//  1. a line ClassifyLine rates CatDangerous is hard-blocked in every mode;
//  2. the mode's shell pre-approval sets the default decision: default and
//     plan pre-approve read-only lines (IsReadOnlyLineFor), auto read-only
//     and build lines (AutoApproveLineFor), bypass everything;
//  3. Manager.Check applies deny → plan → bypass → ask/allow → mode default.
//
// DBypass is reported as DAllow: both run without asking.
func decide(s scenario, cmd string, kind ShellKind, ro, auto bool) Decision {
	if corpusCls.ClassifyLine(cmd) == CatDangerous {
		return DDeny
	}
	def := DAsk
	switch s.mode {
	case Default, Plan:
		if ro {
			def = DAllow
		}
	case Auto:
		if auto {
			def = DAllow
		}
	case Bypass:
		def = DAllow
	}
	d, _ := scenarioManager(s.name, kind).Check("bash", map[string]any{"command": cmd}, def)
	if d == DBypass {
		d = DAllow
	}
	return d
}

func rememberOf(cmd string, kind ShellKind) rememberKind {
	rules, ok := ShellRememberRules("bash", cmd, kind)
	if !ok || len(rules) == 0 {
		return rememberNone
	}
	prefix, group := false, false
	for _, r := range rules {
		if r.CommandGroup != "" {
			group = true
		} else if r.CommandPrefix != "" {
			prefix = true
		}
	}
	switch {
	case prefix && group:
		return rememberMixed
	case group:
		return rememberGroup
	case prefix:
		return rememberPrefix
	}
	return rememberNone
}

// observe runs the six outlets for cmd under kind.
func observe(cmd string, kind ShellKind) permGot {
	g := permGot{
		readOnly: corpusCls.IsReadOnlyLineFor(cmd, kind),
		auto:     corpusCls.AutoApproveLineFor(cmd, kind),
		cat:      corpusCls.ClassifyLineFor(cmd, kind),
		remember: rememberOf(cmd, kind),
		check:    map[string]Decision{},
	}
	_, g.catastrophic = safety.CatastrophicCommand(cmd)
	for _, s := range scenarios {
		g.check[s.name] = decide(s, cmd, kind, g.readOnly, g.auto)
	}
	return g
}

func (g permGot) equal(o permGot) bool {
	if g.readOnly != o.readOnly || g.auto != o.auto || g.cat != o.cat ||
		g.remember != o.remember || g.catastrophic != o.catastrophic {
		return false
	}
	for k, v := range g.check {
		if o.check[k] != v {
			return false
		}
	}
	return true
}

// onlyRememberDiffers reports two observations that differ in remember only.
func onlyRememberDiffers(a, b permGot) bool {
	b.remember = a.remember
	return a.equal(b)
}

func (g permGot) String() string {
	names := make([]string, 0, len(g.check))
	for k := range g.check {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "{readOnly=%v auto=%v cat=%v remember=%v catastrophic=%v check:", g.readOnly, g.auto, g.cat, g.remember, g.catastrophic)
	for _, n := range names {
		fmt.Fprintf(&b, " %s=%s", n, g.check[n])
	}
	b.WriteString("}")
	return b.String()
}

// reporter collects failures with the input, the kind, the outlet,
// got/want and the case note (spec §4.1, §6). Failures are collected rather
// than reported at once so that a case whose only failures are known product
// bugs (knownBugs) is skipped as such instead of failing.
type reporter struct {
	t     testing.TB
	cmd   string
	kind  ShellKind
	note  string
	fails *[]failure
}

// failure is one mismatch or invariant violation. key names the outlet
// ("ShellRememberRules") or invariant ("inv1:remember") for knownBugs.
type failure struct {
	key  string
	kind ShellKind
	got  permGot
	msg  string
}

func (r reporter) add(key string, got permGot, msg string) {
	*r.fails = append(*r.fails, failure{key: key, kind: r.kind, got: got, msg: msg})
}

func (r reporter) mismatch(outlet string, got permGot, g, want any) {
	r.add(outlet, got, fmt.Sprintf("cmd=%q kind=%s outlet=%s: got %v, want %v (note: %s)", r.cmd, r.kind, outlet, g, want, r.note))
}

func (r reporter) invariant(key, detail string, got permGot) {
	r.add(key, got, fmt.Sprintf("cmd=%q kind=%s invariant %s violated: %s; outlets %v (note: %s)", r.cmd, r.kind, key, detail, got, r.note))
}

// knownBug describes a product bug found by the corpus: matches reports
// whether a failure is that bug. A case whose failures are all known bugs is
// skipped with "bug: ..." (spec §6); any other failure is reported.
type knownBug struct {
	desc    string
	matches func(cmd string, f failure) bool
}

// knownBugs is empty: the bugs the corpus found (catastrophic lines that
// were still rememberable, eval/iex/xargs/find -exec and downloaded code
// through a substitution escaping the hard block, a variable program at top
// level passing deny rules, quoted fork-bomb text hard-blocked) are fixed. A
// new entry must name the bug and be removed with its fix.
var knownBugs []knownBug

// settle reports the collected failures of one case, or skips the case when
// every failure is a known product bug.
func settle(t *testing.T, cmd string, fails []failure) {
	t.Helper()
	if len(fails) == 0 {
		return
	}
	var bugs []string
	for _, f := range fails {
		found := ""
		for _, b := range knownBugs {
			if b.matches(cmd, f) {
				found = b.desc
				break
			}
		}
		if found == "" {
			for _, f := range fails {
				t.Error(f.msg)
			}
			return
		}
		bugs = appendUnique(bugs, found)
	}
	t.Skip(strings.Join(bugs, "; "))
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// checkWant compares got with want on every outlet.
func checkWant(r reporter, got permGot, want permWant) {
	if got.readOnly != want.readOnly {
		r.mismatch("IsReadOnlyLineFor", got, got.readOnly, want.readOnly)
	}
	if got.auto != want.auto {
		r.mismatch("AutoApproveLineFor", got, got.auto, want.auto)
	}
	if got.cat != want.cat {
		r.mismatch("ClassifyLineFor", got, got.cat, want.cat)
	}
	if got.remember != want.remember {
		r.mismatch("ShellRememberRules", got, got.remember, want.remember)
	}
	if got.catastrophic != want.catastrophic {
		r.mismatch("CatastrophicCommand", got, got.catastrophic, want.catastrophic)
	}
	names := make([]string, 0, len(want.check))
	for k := range want.check {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, known := got.check[n]; !known {
			r.t.Fatalf("cmd=%q: unknown scenario %q in want.check (note: %s)", r.cmd, n, r.note)
		}
		if got.check[n] != want.check[n] {
			r.mismatch("Check["+n+"]", got, got.check[n], want.check[n])
		}
	}
}

// lineRunsFetch reports a curl or wget command in the line: CatSafe for the
// classifier but network egress, so not read-only (IsReadOnlyLineFor's
// documented exception to invariant 4).
func lineRunsFetch(cmd string) bool {
	for _, sc := range safety.SimpleCommands(cmd) {
		if len(sc.Words) > 0 {
			switch safety.ProgramName(sc.Words[0]) {
			case "curl", "wget":
				return true
			}
		}
	}
	return false
}

// checkInvariants checks invariants 1–4 of spec §4.2 on one observation.
func checkInvariants(r reporter, cmd string, got permGot) {
	// 1. readOnly ⇒ auto; catastrophic ⇒ CatDangerous ∧ ¬readOnly ∧ ¬auto ∧ remember none.
	if got.readOnly && !got.auto {
		r.invariant("inv1", "readOnly but not auto-approved", got)
	}
	if got.catastrophic && (got.cat != CatDangerous || got.readOnly || got.auto) {
		r.invariant("inv1", "catastrophic but not CatDangerous or approved", got)
	}
	if got.catastrophic && got.remember != rememberNone {
		r.invariant("inv1:remember", "catastrophic but [a]/[p] would remember "+got.remember.String(), got)
	}
	// 2. hostile characters ⇒ ¬readOnly ∧ ¬auto ∧ remember none.
	if safety.HasHostileCharacters(cmd) && (got.readOnly || got.auto || got.remember != rememberNone) {
		r.invariant("inv2", "hostile characters but approved or rememberable", got)
	}
	// 3. catastrophic ⇒ deny in all eight scenarios, bypass included.
	if got.catastrophic {
		for _, s := range scenarios {
			if got.check[s.name] != DDeny {
				r.invariant("inv3", fmt.Sprintf("catastrophic but %s decides %s", s.name, got.check[s.name]), got)
			}
		}
	}
	// 4. CatSafe ⇒ readOnly. Documented exceptions: under cmd.exe nothing is
	// read-only (manual: 回退到 cmd.exe 时不自动放行), and curl/wget are
	// CatSafe but network egress (manual: curl/wget 即使只是 GET 也询问).
	if got.cat == CatSafe && !got.readOnly && r.kind != ShellCmd && !lineRunsFetch(cmd) {
		r.invariant("inv4", "CatSafe but not read-only", got)
	}
}

// runCase runs one case under its kinds.
func runCase(t *testing.T, c permCase) {
	t.Helper()
	if c.note == "" {
		t.Errorf("cmd=%q: corpus case without a note", c.cmd)
	}
	if c.skip != "" {
		t.Skip(c.skip)
	}
	kinds := c.kinds
	if kinds == nil {
		kinds = allKinds
	}
	var fails []failure
	obs := map[ShellKind]permGot{}
	for _, k := range kinds {
		got := observe(c.cmd, k)
		obs[k] = got
		r := reporter{t: t, cmd: c.cmd, kind: k, note: c.note, fails: &fails}
		checkWant(r, got, c.want)
		checkInvariants(r, c.cmd, got)
	}
	// 5. A case without kinds must see the same outlets under all three.
	if c.kinds == nil {
		base := obs[allKinds[0]]
		for _, k := range allKinds[1:] {
			if !obs[k].equal(base) {
				key := "inv5"
				if onlyRememberDiffers(base, obs[k]) {
					key = "inv5:remember"
				}
				fails = append(fails, failure{key: key, kind: k, got: obs[k], msg: fmt.Sprintf(
					"cmd=%q invariant 5 violated: 该用例需要按 shell 拆分 — %s %v vs %s %v (note: %s)",
					c.cmd, allKinds[0], base, k, obs[k], c.note)})
			}
		}
	}
	settle(t, c.cmd, fails)
}

// caseName is a readable, bounded subtest name for a command.
func caseName(i int, cmd string) string {
	var b strings.Builder
	for _, r := range cmd {
		switch {
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r > 0x7e:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
		if b.Len() > 48 {
			b.WriteString("…")
			break
		}
	}
	return fmt.Sprintf("%03d_%s", i, b.String())
}

// runCorpus runs every case of one corpus slice as a subtest.
func runCorpus(t *testing.T, cases []permCase) {
	t.Helper()
	for i, c := range cases {
		t.Run(caseName(i, c.cmd), func(t *testing.T) { runCase(t, c) })
	}
}

// corpora lists every corpus slice by file/dimension.
func corpora() []struct {
	name  string
	cases []permCase
} {
	return []struct {
		name  string
		cases []permCase
	}{
		{"readonly", corpusReadOnly},
		{"build", corpusBuild},
		{"write", corpusWrite},
		{"catastrophic", corpusCatastrophic},
		{"wrappers", corpusWrappers},
		{"whitespace", corpusWhitespace},
		{"shellkind", corpusShellKind},
		{"rules", corpusRules},
	}
}

// TestCorpus runs every corpus slice (spec §4.2).
func TestCorpus(t *testing.T) {
	for _, c := range corpora() {
		t.Run(c.name, func(t *testing.T) { runCorpus(t, c.cases) })
	}
}

// TestCorpusDistinct guards the corpus itself: no command appears twice for
// the same shell kind, so the counts in the report are distinct cases.
func TestCorpusDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, c := range corpora() {
		for _, pc := range c.cases {
			kinds := pc.kinds
			if kinds == nil {
				kinds = allKinds
			}
			for _, k := range kinds {
				key := string(k) + "\x00" + pc.cmd
				if prev, dup := seen[key]; dup {
					t.Errorf("cmd=%q kind=%s appears in both %s and %s", pc.cmd, k, prev, c.name)
				}
				seen[key] = c.name
			}
		}
	}
}

// ---- expectation builders ----
//
// Most corpus entries fall into a handful of profiles. The builders only
// fill in a want; each corpus entry still names its command, kinds and note,
// and may override outlets or scenario decisions.

// with returns w with the given scenario decisions added or replaced.
func (w permWant) with(kv ...any) permWant {
	out := w
	out.check = map[string]Decision{}
	for k, v := range w.check {
		out.check[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out.check[kv[i].(string)] = kv[i+1].(Decision)
	}
	return out
}

// rem returns w with remember replaced.
func (w permWant) rem(r rememberKind) permWant { w.remember = r; return w }

// wantReadOnly: a read-only line under POSIX or PowerShell. It runs unasked
// in default, auto and plan, is never remembered (nothing to remember), and
// the rule scenarios leave it alone.
func wantReadOnly() permWant {
	return permWant{readOnly: true, auto: true, cat: CatSafe, check: map[string]Decision{
		scDefault: DAllow, scAuto: DAllow, scPlan: DAllow, scBypass: DAllow,
		scAllowGo: DAllow, scDenyPush: DAllow, scAskGit: DAllow, scParamMatch: DAllow,
	}}
}

// wantCmdReadOnly: a read-only command under the cmd.exe fallback, where
// nothing is pre-approved: it asks, plan refuses it, and [a]/[p] remember
// its prefix. param-match and allow-prefix only allow what they match.
func wantCmdReadOnly() permWant {
	return permWant{cat: CatSafe, remember: rememberPrefix, check: map[string]Decision{
		scDefault: DAsk, scAuto: DAsk, scPlan: DDeny, scBypass: DAllow,
		scAllowGo: DAsk, scDenyPush: DAllow, scAskGit: DAsk, scParamMatch: DAsk,
	}}
}

// wantBuild: a build/test line under POSIX or PowerShell: asks in default,
// runs in auto, refused in plan.
func wantBuild() permWant {
	return permWant{auto: true, cat: CatBuild, remember: rememberPrefix, check: map[string]Decision{
		scDefault: DAsk, scAuto: DAllow, scPlan: DDeny, scBypass: DAllow,
		scDenyPush: DAllow, scAskGit: DAllow, scParamMatch: DAsk,
	}}
}

// wantCmdBuild: a build line under cmd.exe: never pre-approved.
func wantCmdBuild() permWant {
	return permWant{cat: CatBuild, remember: rememberPrefix, check: map[string]Decision{
		scDefault: DAsk, scAuto: DAsk, scPlan: DDeny, scBypass: DAllow,
		scDenyPush: DAllow, scAskGit: DAsk, scParamMatch: DAsk,
	}}
}

// wantAsk: a line that needs approval in every mode (a write, an install,
// an unknown program); plan refuses it; only bypass and allow-all run it.
func wantAsk(cat CmdCategory, r rememberKind) permWant {
	return permWant{cat: cat, remember: r, check: map[string]Decision{
		scDefault: DAsk, scAuto: DAsk, scPlan: DDeny, scBypass: DAllow,
		scAllowGo: DAsk, scDenyPush: DAllow, scAskGit: DAsk, scParamMatch: DAsk,
	}}
}

// ask is a case, the same under all three kinds, of a line that needs
// approval in every mode (wantAsk).
func ask(cmd string, cat CmdCategory, r rememberKind, note string) permCase {
	return permCase{cmd: cmd, want: wantAsk(cat, r), note: note}
}

// push is ask for a git push line: deny-git-push refuses it.
func push(cmd string, r rememberKind, note string) permCase {
	c := ask(cmd, CatGit, r, note)
	c.want = c.want.with(scDenyPush, DDeny)
	return c
}

// wantCatastrophic: hard-blocked in every scenario.
func wantCatastrophic() permWant {
	return permWant{catastrophic: true, cat: CatDangerous, check: map[string]Decision{
		scDefault: DDeny, scAuto: DDeny, scPlan: DDeny, scBypass: DDeny,
		scAllowGo: DDeny, scDenyPush: DDeny, scAskGit: DDeny, scParamMatch: DDeny,
	}}
}

// pairOpt adjusts the wants of a ro()/build() pair: cmdOnly options touch
// only the cmd.exe half, the others both halves.
type pairOpt struct {
	cmdOnly bool
	apply   func(*permWant)
}

// cmdRem sets what [a]/[p] remembers under cmd.exe.
func cmdRem(r rememberKind) pairOpt {
	return pairOpt{true, func(w *permWant) { w.remember = r }}
}

// cmdDecides sets a scenario decision under cmd.exe.
func cmdDecides(name string, d Decision) pairOpt {
	return pairOpt{true, func(w *permWant) { *w = w.with(name, d) }}
}

// remAll sets what [a]/[p] remembers under every kind.
func remAll(r rememberKind) pairOpt {
	return pairOpt{false, func(w *permWant) { w.remember = r }}
}

// decides sets a scenario decision under every kind.
func decides(name string, d Decision) pairOpt {
	return pairOpt{false, func(w *permWant) { *w = w.with(name, d) }}
}

// pair builds the POSIX+PowerShell and cmd.exe cases of one command.
func pair(cmd, note string, posixPS, cmdW permWant, opts []pairOpt) []permCase {
	for _, o := range opts {
		if !o.cmdOnly {
			o.apply(&posixPS)
		}
		o.apply(&cmdW)
	}
	return []permCase{
		{cmd: cmd, kinds: kPosixPS, want: posixPS, note: note},
		{cmd: cmd, kinds: kCmd, want: cmdW, note: note + "（cmd 回退不免询问）"},
	}
}

// ro expands to the two cases of a read-only command: read-only under
// POSIX and PowerShell, asking under cmd.exe.
func ro(cmd, note string, opts ...pairOpt) []permCase {
	return pair(cmd, note, wantReadOnly(), wantCmdReadOnly(), opts)
}

// build expands to the two cases of a build/test command: auto-approved
// under POSIX and PowerShell, asking under cmd.exe. [a]/[p] remember a
// prefix unless remAll says otherwise; allow-prefix-go-test asks unless
// decides says otherwise.
func build(cmd, note string, opts ...pairOpt) []permCase {
	return pair(cmd, note, wantBuild().with(scAllowGo, DAsk), wantCmdBuild().with(scAllowGo, DAsk), opts)
}

// join concatenates case lists.
func join(parts ...[]permCase) []permCase {
	var out []permCase
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
