package tool

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/mcp"
)

// fakeMCPPool is an in-memory mcpPoolView.
type fakeMCPPool struct {
	tools   []mcp.ToolRef
	servers []*mcp.ManagedServer
	result  *mcp.CallToolResult
	read    *mcp.ReadResourceResult
}

func (f *fakeMCPPool) AllTools() []mcp.ToolRef          { return f.tools }
func (f *fakeMCPPool) AllServers() []*mcp.ManagedServer { return f.servers }
func (f *fakeMCPPool) CallTool(context.Context, string, string, map[string]any) (*mcp.CallToolResult, error) {
	return f.result, nil
}
func (f *fakeMCPPool) ReadResource(context.Context, string, string) (*mcp.ReadResourceResult, error) {
	return f.read, nil
}

// TestMCPToolDescriptionCarriesInputSchemas: every MCP tool is reached through
// the single "mcp" proxy, whose own schema only says "arguments: object". The
// description listed tool names and descriptions but not their parameters, so
// the model had to guess argument names - and guessed wrong, costing a failed
// call and a retry at best.
func TestMCPToolDescriptionCarriesInputSchemas(t *testing.T) {
	pool := &fakeMCPPool{tools: []mcp.ToolRef{{
		Server: "fs",
		Tool: mcp.Tool{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}, "tail": map[string]any{"type": "integer"}},
				"required":   []any{"path"},
			},
		},
	}}}
	desc := NewMCPTool(pool).Def().Description
	for _, want := range []string{"[fs] read_file: Read a file", `"path"`, `"tail"`, `"required":["path"]`} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q:\n%s", want, desc)
		}
	}
}

// TestMCPToolDescriptionIsBounded: server-supplied descriptions are sent with
// every single request. One verbose (or hostile) server could add tens of KB
// to each prompt; each tool's entry is clipped.
func TestMCPToolDescriptionIsBounded(t *testing.T) {
	huge := strings.Repeat("very long description ", 2000)
	pool := &fakeMCPPool{tools: []mcp.ToolRef{{Server: "s", Tool: mcp.Tool{Name: "t", Description: huge}}}}
	desc := NewMCPTool(pool).Def().Description
	if len(desc) > 8*1024 {
		t.Fatalf("description is %d bytes for one tool; want each tool's entry clipped", len(desc))
	}
}

func callMCP(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	out, err := NewMCPTool(&fakeMCPPool{result: res}).Call(context.Background(),
		Input{"serverName": "s", "toolName": "t"}, Context{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return out.Data
}

// TestMCPToolResultKeepsTextBlocksApart: several text blocks were glued
// together with no separator ("line oneline two").
func TestMCPToolResultKeepsTextBlocksApart(t *testing.T) {
	got := callMCP(t, &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "text", Text: "line one"}, {Type: "text", Text: "line two"},
	}})
	if !strings.Contains(got, "line one\nline two") {
		t.Fatalf("result = %q; want the blocks on separate lines", got)
	}
}

// TestMCPToolResultDescribesBinaryInsteadOfDumpingIt: an image block was
// rendered as its first 200 bytes of base64 - meaningless to the model and
// pure token noise. It must be described (type, mime, size) instead.
func TestMCPToolResultDescribesBinaryInsteadOfDumpingIt(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(make([]byte, 3000))
	got := callMCP(t, &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "image", MimeType: "image/png", Data: data},
	}})
	if strings.Contains(got, data[:40]) {
		t.Fatalf("result contains raw base64: %q", got)
	}
	if !strings.Contains(got, "image") || !strings.Contains(got, "image/png") || !strings.Contains(got, "3000 bytes") {
		t.Fatalf("result = %q; want the image described with its mime type and size", got)
	}
}

// TestMCPToolResultShowsEmbeddedResources: tools often return files as
// embedded resources ({"type":"resource","resource":{"uri","text"}}) or links.
// Their content was lost - the model saw "[resource: ]".
func TestMCPToolResultShowsEmbeddedResources(t *testing.T) {
	got := callMCP(t, &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "resource", Resource: &mcp.ContentBlock{URI: "file:///a.txt", Text: "embedded body"}},
		{Type: "resource_link", URI: "file:///b.txt", Name: "b.txt"},
	}})
	for _, want := range []string{"file:///a.txt", "embedded body", "file:///b.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("result lacks %q: %q", want, got)
		}
	}
}

func readResource(t *testing.T, res *mcp.ReadResourceResult) string {
	t.Helper()
	out, err := NewReadMCPResourceTool(&fakeMCPPool{read: res}).Call(context.Background(),
		Input{"serverName": "s", "uri": "x://y"}, Context{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return out.Data
}

// TestReadMCPResourceDecodesTextBlobs: resource contents carry binary data in
// "blob", not "data", so blob resources came back as "returned 1 content
// blocks". A blob that is really text (JSON, CSV served as blob) is decoded.
func TestReadMCPResourceDecodesTextBlobs(t *testing.T) {
	got := readResource(t, &mcp.ReadResourceResult{Contents: []mcp.ContentBlock{
		{URI: "x://y", MimeType: "application/json", Blob: base64.StdEncoding.EncodeToString([]byte(`{"k":"v"}`))},
	}})
	if !strings.Contains(got, `{"k":"v"}`) {
		t.Fatalf("result = %q; want the decoded blob", got)
	}
}

func TestReadMCPResourceDescribesBinaryBlobs(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G', 0, 0, 0xff, 0xfe}
	blob := base64.StdEncoding.EncodeToString(raw)
	got := readResource(t, &mcp.ReadResourceResult{Contents: []mcp.ContentBlock{
		{URI: "x://img", MimeType: "image/png", Blob: blob},
	}})
	if strings.Contains(got, blob) {
		t.Fatalf("result dumps the base64 blob: %q", got)
	}
	if !strings.Contains(got, "image/png") || !strings.Contains(got, "8 bytes") {
		t.Fatalf("result = %q; want the blob described", got)
	}
}

// TestListMCPResourcesShowsFailedServers: a server that failed to connect is
// listed with its error, so the model can tell the user why its tools are
// missing instead of claiming there are no servers.
func TestListMCPResourcesShowsFailedServers(t *testing.T) {
	pool := &fakeMCPPool{servers: []*mcp.ManagedServer{{Name: "broken", Err: "start npx: executable file not found"}}}
	out, _ := NewListMCPResourcesTool(pool).Call(context.Background(), Input{}, Context{})
	if !strings.Contains(out.Data, "broken") || !strings.Contains(out.Data, "not connected") || !strings.Contains(out.Data, "executable file not found") {
		t.Fatalf("listing = %q; want the failed server and its error", out.Data)
	}
}
