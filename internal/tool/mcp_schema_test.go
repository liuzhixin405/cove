package tool

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func bigSchema(nProps int, descLen int) map[string]any {
	props := map[string]any{}
	for i := 0; i < nProps; i++ {
		props[fmt.Sprintf("p%02d", i)] = map[string]any{"type": "string", "description": strings.Repeat("d", descLen)}
	}
	// A property literally named "description" must survive the strip.
	props["description"] = map[string]any{"type": "string"}
	return map[string]any{"type": "object", "description": strings.Repeat("top", 100), "properties": props, "required": []any{"p00"}}
}

// An oversized schema drops its description fields first and is never cut in
// the middle of the JSON.
func TestCompactMCPSchemaDropsDescriptionsFirst(t *testing.T) {
	out := compactMCPSchema(bigSchema(20, 300), maxMCPToolSchema)
	if len(out) > maxMCPToolSchema {
		t.Fatalf("len %d > %d", len(out), maxMCPToolSchema)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if strings.Contains(out, "ddd") || strings.Contains(out, "toptop") {
		t.Fatal("descriptions kept")
	}
	props := v["properties"].(map[string]any)
	if _, ok := props["description"]; !ok || len(props) != 21 {
		t.Fatalf("properties damaged: %d", len(props))
	}
}

func TestCompactMCPSchemaSmallUnchanged(t *testing.T) {
	s := map[string]any{"type": "object", "description": "keep me"}
	if out := compactMCPSchema(s, maxMCPToolSchema); !strings.Contains(out, "keep me") {
		t.Fatalf("small schema changed: %s", out)
	}
}

func TestCompactMCPSchemaHugeFallsBackToNames(t *testing.T) {
	out := compactMCPSchema(bigSchema(400, 10), 1024)
	if len(out) > 1024 {
		t.Fatalf("len %d", len(out))
	}
	if strings.HasSuffix(out, "...") && strings.HasPrefix(out, "{") {
		t.Fatalf("clipped mid-JSON: %s", out)
	}
	if !strings.Contains(out, "p00") {
		t.Fatalf("should at least name properties: %s", out)
	}
}
