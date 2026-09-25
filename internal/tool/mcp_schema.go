package tool

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// compactMCPSchema renders an MCP tool's input schema for the proxy
// description within limit bytes without cutting the JSON in the middle (a
// half object only misleads the model). Past limit it tries, in order: the
// schema without its description strings, the property names with their
// types only, and finally a plain list of property names.
func compactMCPSchema(schema map[string]any, limit int) string {
	if s, ok := marshalWithin(schema, limit); ok {
		return s
	}
	stripped, _ := stripSchemaDescriptions(schema).(map[string]any)
	if s, ok := marshalWithin(stripped, limit); ok {
		return s
	}
	props, _ := stripped["properties"].(map[string]any)
	typesOnly := map[string]any{}
	for k, v := range stripped {
		if k == "type" || k == "required" {
			typesOnly[k] = v
		}
	}
	if props != nil {
		p := map[string]any{}
		for name, def := range props {
			entry := map[string]any{}
			if m, ok := def.(map[string]any); ok {
				if t, ok := m["type"]; ok {
					entry["type"] = t
				}
			}
			p[name] = entry
		}
		typesOnly["properties"] = p
	}
	if s, ok := marshalWithin(typesOnly, limit); ok {
		return s
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	var sb strings.Builder
	sb.WriteString("(schema too large to include; properties: ")
	for i, n := range names {
		more := fmt.Sprintf(" ... (+%d more)", len(names)-i)
		sep := ""
		if i > 0 {
			sep = ", "
		}
		if sb.Len()+len(sep)+len(n)+len(more)+1 > limit {
			sb.WriteString(more)
			break
		}
		sb.WriteString(sep + n)
	}
	sb.WriteString(")")
	return sb.String()
}

func marshalWithin(v any, limit int) (string, bool) {
	data, err := json.Marshal(v)
	if err != nil || len(data) > limit {
		return "", false
	}
	return string(data), true
}

// stripSchemaDescriptions returns a copy of a JSON-schema value without its
// "description" strings. Only string-valued keys are dropped, so a property
// literally named "description" (an object) is kept.
func stripSchemaDescriptions(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if _, isStr := val.(string); isStr && k == "description" {
				continue
			}
			out[k] = stripSchemaDescriptions(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripSchemaDescriptions(val)
		}
		return out
	}
	return v
}
