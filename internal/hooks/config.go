package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// defaultConfigHookTimeout bounds a configured hook that sets no timeout of
// its own. A sequential BeforeTool hook holds up the tool call, so a hook that
// hangs must not hold it up forever.
const defaultConfigHookTimeout = 60 * time.Second

// HookDef is one hook from the user's ~/.cove/hooks.json:
//
//	{"hooks": {"BeforeTool": [{"matcher": "bash", "command": "..."}]}}
//
// Command is a command line run by the same shell as the bash tool (Git Bash,
// PowerShell or cmd on Windows; bash/sh elsewhere). It receives the HookInput
// as JSON on stdin and may print a HookOutput as JSON; {"continue": false}
// from a BeforeTool hook blocks the tool call.
type HookDef struct {
	Event   HookEvent `json:"-"`
	Matcher string    `json:"matcher,omitempty"` // regexp on the whole tool name; empty or "*" = all
	Command string    `json:"command"`
	Timeout int       `json:"timeout,omitempty"` // seconds; 0 = defaultConfigHookTimeout
	Async   bool      `json:"async,omitempty"`   // fire-and-forget; cannot block
}

// Config converts the definition into the HookConfig the manager runs.
func (d HookDef) Config() HookConfig {
	timeout := defaultConfigHookTimeout
	if d.Timeout > 0 {
		timeout = time.Duration(d.Timeout) * time.Second
	}
	return HookConfig{
		Event:      d.Event,
		Matcher:    anchorMatcher(d.Matcher),
		Type:       HookCommand,
		Command:    d.Command,
		Shell:      true,
		Timeout:    timeout,
		Sequential: !d.Async,
	}
}

// anchorMatcher turns a configured matcher into the regexp the manager runs.
// It names tools, so it must match the whole name: unanchored, "bash" also
// ran for "bash_output" and "read|write" for "readme". A matcher that already
// uses ^ or $ is taken as written, and "*" (not a valid regexp) means all.
func anchorMatcher(m string) string {
	m = strings.TrimSpace(m)
	switch {
	case m == "" || m == "*":
		return ""
	case strings.HasPrefix(m, "^") || strings.HasSuffix(m, "$"):
		return m
	default:
		return "^(?:" + m + ")$"
	}
}

// configEvents maps the event names accepted in hooks.json to events. The
// PreToolUse/PostToolUse spellings used by other assistants are accepted too.
var configEvents = map[string]HookEvent{
	"BeforeTool":   BeforeTool,
	"AfterTool":    AfterTool,
	"PreToolUse":   BeforeTool,
	"PostToolUse":  AfterTool,
	"SessionStart": SessionStart,
	"SessionEnd":   SessionEnd,
}

// LoadUserConfig reads home/.cove/hooks.json. A missing file means no hooks
// and no error. Hooks are only ever read from the user's own home directory:
// a project-level hooks file would let a cloned repository run commands on
// the user's machine.
//
// Entries that are invalid (unknown event, empty command, bad matcher) are
// reported in the returned error while the valid ones are still returned, so
// one typo does not disable every hook.
func LoadUserConfig(home string) ([]HookDef, error) {
	return LoadConfigDir(filepath.Join(home, ".cove"))
}

// LoadConfigDir reads dir/hooks.json, with LoadUserConfig's semantics. dir
// is the user's config directory (config.ConfigDir: COVE_CONFIG_DIR, else
// ~/.cove), never a project directory.
func LoadConfigDir(dir string) ([]HookDef, error) {
	path := filepath.Join(dir, "hooks.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Hooks map[string][]HookDef `json:"hooks"`
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	names := make([]string, 0, len(file.Hooks))
	for name := range file.Hooks {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order and error text

	var defs []HookDef
	var errs []error
	for _, name := range names {
		event, ok := configEvents[name]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: unknown hook event %q", path, name))
			continue
		}
		for i, d := range file.Hooks[name] {
			d.Event = event
			if d.Command == "" {
				errs = append(errs, fmt.Errorf("%s: %s[%d]: empty command", path, name, i))
				continue
			}
			if d.Matcher != "" {
				if _, err := regexp.Compile(anchorMatcher(d.Matcher)); err != nil {
					errs = append(errs, fmt.Errorf("%s: %s[%d]: invalid matcher: %w", path, name, i, err))
					continue
				}
			}
			defs = append(defs, d)
		}
	}
	return defs, errors.Join(errs...)
}

// Register adds a hook to the manager.
func (m *Manager) Register(h HookConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks[h.Event] = append(m.hooks[h.Event], h)
}

// RegisterDefs registers every configured hook.
func (m *Manager) RegisterDefs(defs []HookDef) {
	for _, d := range defs {
		m.Register(d.Config())
	}
}
