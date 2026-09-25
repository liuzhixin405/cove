package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/textutil"
)

type mcpPoolView interface {
	AllTools() []mcp.ToolRef
	AllServers() []*mcp.ManagedServer
	CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (*mcp.CallToolResult, error)
	ReadResource(ctx context.Context, serverName, uri string) (*mcp.ReadResourceResult, error)
}

func NewMCPTool(pool mcpPoolView) Tool {
	return &mcpToolProxy{pool: pool, baseTool: baseTool{def: Def{
		Name:        "mcp",
		Description: "Invoke tools from connected MCP servers. Use when you need capabilities provided by external tools.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"serverName":{"type":"string","description":"MCP server name"},
				"toolName":{"type":"string","description":"Tool name on the MCP server"},
				"arguments":{"type":"object","description":"Arguments to pass to the tool"}
			},
			"required":["serverName","toolName"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: true, UserFacingName: "MCP Tool",
	}}}
}

type mcpToolProxy struct {
	baseTool
	pool mcpPoolView
}

// Bounds on what one server-supplied tool adds to the description. It is sent
// with every request, so a verbose (or hostile) server could otherwise add tens
// of KB to each prompt.
const (
	maxMCPToolDescription = 2048
	maxMCPToolSchema      = 4096
)

// Def lists every connected tool with its input schema. All MCP tools are
// reached through this one proxy, whose own schema only says "arguments:
// object"; without the per-tool schemas the model had to guess argument names.
// The pool lists tools in a stable order, which keeps the description - and
// with it the provider's prompt cache - identical from turn to turn.
func (t *mcpToolProxy) Def() Def {
	d := t.def
	if t.pool == nil {
		return d
	}
	var sb strings.Builder
	sb.WriteString(d.Description)
	for _, tr := range t.pool.AllTools() {
		fmt.Fprintf(&sb, "\n  [%s] %s: %s", tr.Server, tr.Tool.Name,
			textutil.ClipBytes(tr.Tool.Description, maxMCPToolDescription, "..."))
		if len(tr.Tool.InputSchema) > 0 {
			// json.Marshal sorts map keys, so this is deterministic too.
			// Oversized schemas lose their descriptions first and are never
			// clipped mid-JSON (compactMCPSchema).
			sb.WriteString("\n    arguments schema: ")
			sb.WriteString(compactMCPSchema(tr.Tool.InputSchema, maxMCPToolSchema))
		}
	}
	d.Description = sb.String()
	return d
}

func (t *mcpToolProxy) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if t.pool == nil {
		return Result{Data: "MCP error: no MCP pool configured", IsError: true}, nil
	}
	serverName, _ := input["serverName"].(string)
	toolName, _ := input["toolName"].(string)
	args, _ := input["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}

	result, err := t.pool.CallTool(ctx, serverName, toolName, args)
	if err != nil {
		return Result{Data: "MCP error: " + err.Error(), IsError: true}, nil
	}
	// A (nil, nil) return would panic on the deref below. The neighbouring
	// readMCPResource already guarded against it; this path did not.
	if result == nil {
		return Result{Data: "MCP error: server returned an empty result", IsError: true}, nil
	}

	parts := make([]string, 0, len(result.Content))
	for _, c := range result.Content {
		parts = append(parts, formatMCPContent(c))
	}
	// Blocks are separate items; they used to be glued together with no
	// separator, merging the last word of one with the first of the next.
	out := strings.Join(parts, "\n")
	if result.IsError {
		out = "[MCP Error] " + out
	}
	return Result{Data: out, IsError: result.IsError}, nil
}

// formatMCPContent renders one tool-result block as text for the model.
//
// Binary blocks are described, not dumped: they used to appear as their first
// 200 bytes of base64, which means nothing to the model and costs tokens.
// Embedded resources and resource links used to lose their content entirely
// ("[resource: ]").
func formatMCPContent(c mcp.ContentBlock) string {
	switch c.Type {
	case "text":
		return c.Text
	case "image", "audio":
		return fmt.Sprintf("[%s %s, %d bytes, not shown]", c.Type, c.MimeType, base64DecodedLen(c.Data))
	case "resource":
		if c.Resource == nil {
			return "[resource]"
		}
		return formatMCPResourceContents(*c.Resource)
	case "resource_link":
		link := "[resource link: " + c.URI
		if c.Name != "" {
			link += " (" + c.Name + ")"
		}
		return link + "] (read it with mcp_read_resource)"
	default:
		if c.Text != "" {
			return c.Text
		}
		return fmt.Sprintf("[%s content, not shown]", c.Type)
	}
}

// formatMCPResourceContents renders resource contents (text or base64 blob).
func formatMCPResourceContents(r mcp.ContentBlock) string {
	var header string
	if r.URI != "" {
		header = "[resource " + r.URI + "]\n"
	}
	if r.Text != "" {
		return header + r.Text
	}
	if r.Blob != "" {
		return header + describeBlob(r)
	}
	return strings.TrimSpace(header)
}

// describeBlob decodes a resource blob that is really text (JSON or CSV served
// as a blob) and describes anything else instead of dumping base64.
func describeBlob(r mcp.ContentBlock) string {
	raw, err := base64.StdEncoding.DecodeString(r.Blob)
	if err == nil && utf8.Valid(raw) && !bytes.ContainsRune(raw, 0) {
		return string(raw)
	}
	n := len(raw)
	if err != nil {
		n = base64DecodedLen(r.Blob)
	}
	mime := r.MimeType
	if mime == "" {
		mime = "unknown type"
	}
	return fmt.Sprintf("[binary resource %s (%s), %d bytes, not shown]", r.URI, mime, n)
}

// base64DecodedLen is the decoded size of standard base64 without decoding it.
func base64DecodedLen(s string) int {
	n := len(s) / 4 * 3
	switch {
	case strings.HasSuffix(s, "=="):
		n -= 2
	case strings.HasSuffix(s, "="):
		n--
	}
	if n < 0 {
		return 0
	}
	return n
}

func (t *mcpToolProxy) CheckPermissions(input Input, tctx Context) PermissionDecision {
	server, _ := input["serverName"].(string)
	tool, _ := input["toolName"].(string)
	return Asked(fmt.Sprintf("MCP: %s/%s requires approval", server, tool))
}

func (t *mcpToolProxy) Validate(input Input) string {
	return ""
}

func NewListMCPResourcesTool(pool mcpPoolView) Tool {
	return &listMCPResources{pool: pool, baseTool: baseTool{def: Def{
		Name:        "mcp_resources",
		Description: "List available resources from connected MCP servers.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"server":{"type":"string","description":"Optional server name filter"}}
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "MCP Resources",
	}}}
}

func NewReadMCPResourceTool(pool mcpPoolView) Tool {
	return &readMCPResource{pool: pool, baseTool: baseTool{def: Def{
		Name:        "mcp_read_resource",
		Description: "Read a resource exposed by a connected MCP server.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"serverName":{"type":"string","description":"MCP server name"},
				"uri":{"type":"string","description":"Resource URI to read"}
			},
			"required":["serverName","uri"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "MCP Read Resource",
	}}}
}

type listMCPResources struct {
	baseTool
	pool mcpPoolView
}

type readMCPResource struct {
	baseTool
	pool mcpPoolView
}

func (t *listMCPResources) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if t.pool == nil {
		return Result{Data: "No MCP servers connected"}, nil
	}
	serverFilter, _ := input["server"].(string)
	var sb strings.Builder
	for _, s := range t.pool.AllServers() {
		if serverFilter != "" && s.Name != serverFilter {
			continue
		}
		if !s.Connected && s.Err != "" {
			// A server that failed to connect is listed with its error so the
			// model can tell the user why its tools are missing, instead of
			// claiming there are no servers.
			fmt.Fprintf(&sb, "[%s] not connected: %s\n", s.Name, s.Err)
			continue
		}
		fmt.Fprintf(&sb, "[%s] %d tools, %d resources\n", s.Name, len(s.Tools), len(s.Resources))
		for _, tool := range s.Tools {
			fmt.Fprintf(&sb, "  tool: %s - %s\n", tool.Name, tool.Description)
		}
		for _, resource := range s.Resources {
			line := fmt.Sprintf("  resource: %s", resource.URI)
			if resource.Name != "" {
				line += fmt.Sprintf(" (%s)", resource.Name)
			}
			if resource.Description != "" {
				line += fmt.Sprintf(" - %s", resource.Description)
			}
			sb.WriteString(line + "\n")
		}
	}
	if sb.Len() == 0 {
		if serverFilter != "" {
			return Result{Data: fmt.Sprintf("No MCP resources found for server: %s", serverFilter)}, nil
		}
		return Result{Data: "No MCP servers connected"}, nil
	}
	return Result{Data: strings.TrimSpace(sb.String())}, nil
}

func (t *listMCPResources) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("mcp_resources is read-only")
}

func (t *listMCPResources) Validate(input Input) string { return "" }

func (t *readMCPResource) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if t.pool == nil {
		return Result{Data: "MCP resource read error: no MCP pool configured", IsError: true}, nil
	}
	serverName, _ := input["serverName"].(string)
	uri, _ := input["uri"].(string)
	result, err := t.pool.ReadResource(ctx, serverName, uri)
	if err != nil {
		return Result{Data: "MCP resource read error: " + err.Error(), IsError: true}, nil
	}
	if result == nil || len(result.Contents) == 0 {
		return Result{Data: fmt.Sprintf("No content for MCP resource: %s", uri)}, nil
	}
	var sb strings.Builder
	for _, block := range result.Contents {
		// Resource contents carry binary data in "blob"; this used to read
		// "data", which resources never set, so blob resources came back as
		// "returned N content blocks".
		var text string
		switch {
		case block.Text != "":
			text = block.Text
		case block.Blob != "":
			text = describeBlob(block)
		case block.Data != "":
			text = formatMCPContent(block)
		default:
			continue
		}
		sb.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			sb.WriteString("\n")
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return Result{Data: fmt.Sprintf("MCP resource %s returned %d content blocks", uri, len(result.Contents))}, nil
	}
	return Result{Data: text}, nil
}

func (t *readMCPResource) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("mcp_read_resource is read-only")
}

func (t *readMCPResource) Validate(input Input) string { return "" }
