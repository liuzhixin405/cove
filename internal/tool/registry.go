package tool

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Registry struct {
	tools   map[string]Tool
	order   []string
	version int
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(t Tool) {
	d := t.Def()
	r.tools[d.Name] = t
	for _, a := range d.Aliases {
		r.tools[a] = t
	}
	r.order = append(r.order, d.Name)
	r.version++
}

// Version returns a counter that increments whenever a tool is registered.
// Callers caching tool metadata can compare against this to detect staleness.
func (r *Registry) Version() int { return r.version }

func (r *Registry) Find(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) All() []Tool {
	seen := map[string]bool{}
	var result []Tool
	for _, name := range r.order {
		t := r.tools[name]
		d := t.Def()
		if !seen[d.Name] {
			seen[d.Name] = true
			result = append(result, t)
		}
	}
	return result
}

func (r *Registry) Enabled(ctx Context) []Tool {
	var res []Tool
	for _, t := range r.All() {
		if t.CheckPermissions(nil, ctx).Decision != Deny {
			res = append(res, t)
		}
	}
	return res
}

func (r *Registry) ToolDefs() []json.RawMessage {
	var out []json.RawMessage
	for _, t := range r.All() {
		d := t.Def()
		schema := d.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, json.RawMessage(fmt.Sprintf(
			`{"name":"%s","description":"%s","input_schema":%s}`,
			d.Name, strings.ReplaceAll(d.Description, `"`, `\"`), string(schema),
		)))
	}
	return out
}
