package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/engine"
	"github.com/agentgo/internal/tool"
)

type Definition struct {
	Name        string
	Description string
	Prompt      string
	Model       string
	Tools       []string
}

type Config struct {
	Model     string
	Provider  api.ProviderConfig
	Tools     []tool.Tool
	MaxBudget float64
	Debug     bool
}

type Runner struct {
	defs   map[string]Definition
	config Config
}

func NewRunner(config Config) *Runner {
	return &Runner{defs: make(map[string]Definition), config: config}
}

func (r *Runner) Register(name, description, prompt string) {
	r.defs[name] = Definition{Name: name, Description: description, Prompt: prompt}
}

func (r *Runner) Run(ctx context.Context, name string, task string) (*api.AgentRunResult, error) {
	def, ok := r.defs[name]
	if !ok {
		return &api.AgentRunResult{Success: false, Error: fmt.Sprintf("agent %s not found", name)}, fmt.Errorf("agent %s not found", name)
	}
	agentTools := r.filterTools(def.Tools)
	eng, err := engine.New(engine.Config{
		Model:          firstNonEmpty(def.Model, r.config.Model),
		PermissionMode: "auto",
		MaxBudget:      r.config.MaxBudget,
		Debug:          r.config.Debug,
		Tools:          agentTools,
		Provider:       r.config.Provider,
	})
	if err != nil {
		return &api.AgentRunResult{Error: err.Error()}, nil
	}

	systemParts := []string{
		fmt.Sprintf("[Sub-agent: %s]", def.Name),
		def.Description,
	}
	if def.Prompt != "" {
		systemParts = append(systemParts, "Instructions: "+def.Prompt)
	}
	systemParts = append(systemParts, "Complete the assigned task and return the result. Be thorough and precise.")
	eng.SetSystemOverride(strings.Join(systemParts, "\n"))

	start := time.Now()
	output, err := eng.Run(ctx, task)
	return &api.AgentRunResult{
		Output:  output,
		Cost:    eng.CostTracker().TotalCost,
		Steps:   int(time.Since(start).Seconds()),
		Success: err == nil,
		Error:   errStr(err),
	}, nil
}

func (r *Runner) filterTools(names []string) []tool.Tool {
	if len(names) == 0 {
		return r.config.Tools
	}
	var result []tool.Tool
	for _, t := range r.config.Tools {
		d := t.Def()
		for _, n := range names {
			if d.Name == n {
				result = append(result, t)
				break
			}
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "claude-sonnet-4-20250514"
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
