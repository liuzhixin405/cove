package tool

import (
	"context"
	"encoding/json"
	"sync"
)

type Input = map[string]any

type Result struct {
	Data        string `json:"data"`
	IsError     bool   `json:"is_error"`
	ShouldRetry bool   `json:"should_retry"`
}

type PermissionDecision struct {
	Decision PermissionResult
	Reason   string
}

type PermissionResult string

const (
	Allow  PermissionResult = "allow"
	Deny   PermissionResult = "deny"
	Ask    PermissionResult = "ask"
	Bypass PermissionResult = "bypass"
)

type Context struct {
	Cwd              string
	ToolUseID        string
	SessionID        string
	PermissionMode   string
	AlwaysAllowRules map[string][]string
	AlwaysDenyRules  map[string][]string
	IsNonInteractive bool
	Debug            bool
	Runtime          *Runtime
	// OnProgress, when set, is invoked with incremental output chunks while a
	// long-running tool (e.g. bash/powershell) executes. It lets the UI show
	// live output and lets the stall monitor know the tool is still alive.
	OnProgress func(chunk string)
}

type Runtime struct {
	mu            sync.Mutex
	PlanMode      bool
	WorktreeDir   string
	Tasks         map[string]*TaskRecord
	Teams         map[string]*TeamRecord
	CronSchedules map[string]*CronRecord
	Messages      []MessageRecord
	TaskCounter   int
	AgentRunner   any
	LSPRunner     LSPRunner
	SkillManager  any
	SkillPrompts  map[string]string
	PluginManager any
	Cwd           string
	AskUser       func(prompt string) string
	// PlanExecuteFunc, when set, is invoked by the execute_plan tool.
	// It receives parallel flag and returns a formatted result summary.
	PlanExecuteFunc func(parallel bool) (string, error)
}

func (r *Runtime) Lock()   { r.mu.Lock() }
func (r *Runtime) Unlock() { r.mu.Unlock() }

// PlanMode and WorktreeDir are written by tool calls and read by others, and
// tool calls can execute concurrently (see the engine's parallel batch). They
// are guarded by the same mutex as the rest of Runtime, so go through these
// accessors rather than touching the fields directly.

// SetPlanMode records whether plan mode is active.
func (r *Runtime) SetPlanMode(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.PlanMode = on
}

// IsPlanMode reports whether plan mode is active.
func (r *Runtime) IsPlanMode() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.PlanMode
}

// SetWorktreeDir records the active worktree path ("" when none).
func (r *Runtime) SetWorktreeDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.WorktreeDir = dir
}

// GetWorktreeDir returns the active worktree path, or "" when none.
func (r *Runtime) GetWorktreeDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.WorktreeDir
}

type LSPRunner interface {
	Run(ctx context.Context, action string, filePath string, input Input) (string, error)
}

type TaskRecord struct {
	ID          string
	Title       string
	Description string
	Status      string
	Output      string
	Kind        string
	ParentID    string
	CreatedAt   string
	UpdatedAt   string
}

type TeamRecord struct {
	Name      string
	Members   []TeamMemberRecord
	Status    string
	CreatedAt string
}

type TeamMemberRecord struct {
	ID     string
	Agent  string
	Task   string
	Status string
	Output string
}

type CronRecord struct {
	ID        string
	Schedule  string
	Task      string
	Status    string
	CreatedAt string
}

type MessageRecord struct {
	To        string
	Message   string
	CreatedAt string
	Delivered bool
}

type Def struct {
	Name              string
	Aliases           []string
	Description       string
	Prompt            string
	InputSchema       json.RawMessage
	IsReadOnly        bool
	IsConcurrencySafe bool
	UserFacingName    string
}

type Tool interface {
	Def() Def
	Call(ctx context.Context, input Input, tctx Context) (Result, error)
	Validate(input Input) string
	CheckPermissions(input Input, tctx Context) PermissionDecision
}

type baseTool struct{ def Def }

func (b *baseTool) Def() Def                    { return b.def }
func (b *baseTool) Validate(input Input) string { return "" }
func (b *baseTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return PermissionDecision{Decision: Deny, Reason: "not implemented"}
}

func Decision(d PermissionResult, reason string) PermissionDecision {
	return PermissionDecision{Decision: d, Reason: reason}
}

func Allowed(reason string) PermissionDecision { return Decision(Allow, reason) }
func Denied(reason string) PermissionDecision  { return Decision(Deny, reason) }
func Asked(reason string) PermissionDecision   { return Decision(Ask, reason) }
