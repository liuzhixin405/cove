package main

import (
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/diagnostic"
	"github.com/liuzhixin405/cove/internal/termui"
)

// diagRuntime is what the diagnostic layer's remedies may do in the
// interactive shell: adjust a model's context budget for this session,
// write a learned window to config.json, and tell the user a line.
type diagRuntime struct{}

func (diagRuntime) SetModelContextWindow(model string, tokens int) {
	api.SetModelContextWindow(model, tokens)
}

// PersistModelContextWindow records tokens as model's window in config.json
// (model_context_windows), so the next start applies it before the first
// model call. Only that key changes: config.Save merges into the file.
func (diagRuntime) PersistModelContextWindow(model string, tokens int) error {
	return persistModelContextWindow(model, tokens)
}

func persistModelContextWindow(model string, tokens int) error {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || tokens <= 0 {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.ModelContextWindows == nil {
		cfg.ModelContextWindows = map[string]int{}
	}
	if cfg.ModelContextWindows[model] == tokens {
		return nil
	}
	cfg.ModelContextWindows[model] = tokens
	return config.Save(cfg)
}

func (diagRuntime) Notify(line string) {
	termui.PrintAbove("  " + termui.Styled(termui.Yellow, "⚙ "+line) + "\n")
}

// installDiagnostics applies the configured context windows before any
// model call and, in the interactive shell only, wires the remedies'
// Runtime: a -p or headless run has no console, and a remedy's line would
// land in the stdout a script is reading.
func installDiagnostics(cfg *config.Config, interactive bool) {
	if interactive {
		diagnostic.SetRuntime(diagRuntime{})
	} else {
		diagnostic.SetRuntime(nil)
	}
	if cfg == nil {
		return
	}
	for model, tokens := range cfg.ModelContextWindows {
		if tokens > 0 {
			api.SetModelContextWindow(model, tokens)
		}
	}
	// The single context_window key names the current model's window and
	// wins over the map for that model.
	if cfg.ContextWindow > 0 && cfg.Model != "" {
		api.SetModelContextWindow(cfg.Model, cfg.ContextWindow)
	}
}
