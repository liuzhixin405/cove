package session

import (
	"reflect"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// Diff reported the two directions swapped: a tool used for the first time
// this turn came back as removed, and one dropped by compaction as added.
func TestDiffReportsNewToolsAndFilesAsAdded(t *testing.T) {
	before := NewSessionView([]api.Message{{Role: "user", Content: "go"}}, 10)
	after := NewSessionView([]api.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "1", Name: "read", Input: map[string]any{"filePath": "a.go"}}}},
	}, 20)

	d := Diff(before, after)
	if !reflect.DeepEqual(d.AddedTools, []string{"read"}) || len(d.RemovedTools) != 0 {
		t.Fatalf("tools added=%v removed=%v, want added=[read]", d.AddedTools, d.RemovedTools)
	}
	if !reflect.DeepEqual(d.AddedFiles, []string{"a.go"}) || len(d.RemovedFiles) != 0 {
		t.Fatalf("files added=%v removed=%v, want added=[a.go]", d.AddedFiles, d.RemovedFiles)
	}
}
