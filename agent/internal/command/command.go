package command

import (
	"context"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/buddy"
	"github.com/agentgo/internal/config"
	ctxt "github.com/agentgo/internal/context"
	"github.com/agentgo/internal/mcp"
	"github.com/agentgo/internal/memory"
	"github.com/agentgo/internal/permission"
	"github.com/agentgo/internal/plugin"
	"github.com/agentgo/internal/session"
	"github.com/agentgo/internal/skills"
	"github.com/agentgo/internal/state"
)

type CostTrackerView interface {
	Summary() string
}

type EngineView interface {
	Messages() []api.Message
	LoadMessages([]api.Message)
	SetSystemOverride(prompt string)
	SystemPrompt() string
	CostTracker() CostTrackerView
}

type SessionStore interface {
	List() ([]session.Record, error)
	Load(id string) (*session.Record, error)
}

type PluginManager interface {
	Install(name string, url string) error
	Uninstall(name string) error
	Disable(name string) error
	Enable(name string) error
	AllPlugins() []plugin.Entry
}

type SkillManager interface {
	All() []skills.Skill
	Get(name string) (skills.Skill, bool)
}

type MemoryStore interface {
	All() []memory.Entry
	Save(name, content string) error
	Delete(name string) error
}

type PermissionManager interface {
	Mode() permission.Mode
	SetMode(mode permission.Mode)
}

type MCPPool interface {
	Connect(ctx context.Context, name string, cfg mcp.ServerConfig) error
	Disconnect(name string)
	DisconnectAll()
	AllServers() []*mcp.ManagedServer
	AllTools() []mcp.ToolRef
	AllResources() []mcp.ResourceRef
	ReadResource(ctx context.Context, serverName, uri string) (*mcp.ReadResourceResult, error)
}

type Input struct {
	Args              []string
	Cwd               string
	Config            *config.Config
	SaveConfig        func(*config.Config) error
	Engine            EngineView
	SessionStore      SessionStore
	PluginManager     PluginManager
	SkillManager      SkillManager
	MemoryStore       MemoryStore
	PermissionManager PermissionManager
	MCPPool           MCPPool
	ProjectContext    *ctxt.ProjectContext
	AppState          *state.AppState
	BuddyDisplay      *buddy.Display
	BuddyChat         *buddy.BuddyChat
	Provider          api.Provider
}

type Output struct {
	Message string
	Data    string
}

type Command interface {
	Name() string
	Aliases() []string
	Description() string
	Help() string
	Execute(ctx context.Context, input Input) (Output, error)
}

type Registry struct {
	cmds  map[string]Command
	order []string
}

func NewRegistry() *Registry {
	return &Registry{cmds: make(map[string]Command)}
}

func (r *Registry) Register(c Command) {
	r.cmds[c.Name()] = c
	for _, a := range c.Aliases() {
		r.cmds[a] = c
	}
	r.order = append(r.order, c.Name())
}

func (r *Registry) Find(name string) (Command, bool) {
	c, ok := r.cmds[name]
	return c, ok
}

func (r *Registry) All() []Command {
	var res []Command
	seen := map[string]bool{}
	for _, n := range r.order {
		c := r.cmds[n]
		if !seen[c.Name()] {
			seen[c.Name()] = true
			res = append(res, c)
		}
	}
	return res
}
