package command

import (
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/session"
)

// Optional engine capabilities. EngineView stays small so tests and other
// front ends can satisfy it; a command that changes something the running
// session depends on looks for these and, when they are missing, says the
// change only applies after a restart instead of claiming it took effect.

// instructionsSetter applies the user's custom instructions (config
// "system_prompt") to the running session. applied is false when the engine
// behind the view cannot do it yet.
type instructionsSetter interface {
	SetCustomInstructions(ci string) (applied bool)
}

// workingDirSetter moves the engine's project directory (checkpoints, the
// session's project, verification commands) to dir after /cd. applied is
// false when the engine behind the view cannot follow.
type workingDirSetter interface {
	SetWorkingDir(dir string) (applied bool)
}

// sessionResumer continues a saved session under its own ID.
type sessionResumer interface {
	ResumeSession(r *session.Record)
}

// providerReloader switches the running session's provider or model.
type providerReloader interface {
	ReloadProvider(provider, model, baseURL, apiKey string) error
}

// permissionModeSetter changes the mode of the engine's own permission
// manager, which is the one that gates tools; the front end's manager is only
// what /permissions reports.
type permissionModeSetter interface {
	SetPermissionMode(mode permission.Mode)
}

// budgetSetter changes the running session's budget cap.
type budgetSetter interface {
	SetMaxBudget(maxBudget float64)
}
