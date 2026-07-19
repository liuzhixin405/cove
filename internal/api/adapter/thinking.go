package adapter

import "strings"

// MergeReasoning appends non-empty reasoning snippets using a newline.
func MergeReasoning(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}
