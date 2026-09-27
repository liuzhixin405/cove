package tool

import (
	"testing"

	"github.com/liuzhixin405/cove/internal/mcp"
)

// A typed nil *mcp.Pool stored in the interface used to pass the nil check
// and panic in Def.
func TestMCPToolDefSurvivesTypedNilPool(t *testing.T) {
	var pool *mcp.Pool
	tl := NewMCPTool(pool)
	if d := tl.Def(); d.Name == "" {
		t.Fatal("Def returned an empty definition")
	}
}
