package engine

import (
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

func TestTouchPathsFor(t *testing.T) {
	cases := []struct {
		name string
		tc   api.ToolCall
		want []string
	}{
		{
			name: "read reports the file it opened",
			tc:   api.ToolCall{Name: "read", Input: map[string]any{"filePath": "pkg/a.go"}},
			want: []string{"pkg/a.go"},
		},
		{
			name: "grep reports the root it searched",
			tc:   api.ToolCall{Name: "grep", Input: map[string]any{"path": "pkg", "pattern": "Foo"}},
			want: []string{"pkg"},
		},
		{
			name: "edit reports the file it changed",
			tc:   api.ToolCall{Name: "edit", Input: map[string]any{"filePath": "pkg/a.go"}},
			want: []string{"pkg/a.go"},
		},
		{
			name: "both filePath and path are reported",
			tc:   api.ToolCall{Name: "read", Input: map[string]any{"filePath": "a.go", "path": "pkg"}},
			want: []string{"a.go", "pkg"},
		},
		{
			name: "bash carries no path of its own",
			tc:   api.ToolCall{Name: "bash", Input: map[string]any{"command": "go test ./..."}},
			want: nil,
		},
		{
			name: "empty values are ignored",
			tc:   api.ToolCall{Name: "read", Input: map[string]any{"filePath": ""}},
			want: nil,
		},
		{
			name: "non-string values are ignored",
			tc:   api.ToolCall{Name: "read", Input: map[string]any{"filePath": 42}},
			want: nil,
		},
		{
			name: "nil input is safe",
			tc:   api.ToolCall{Name: "read"},
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := touchPathsFor(c.tc)
			if len(got) != len(c.want) {
				t.Fatalf("touchPathsFor() = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("touchPathsFor() = %v, want %v", got, c.want)
				}
			}
		})
	}
}
