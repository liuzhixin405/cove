package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentgo/internal/mcp"
)

type MCPTool struct{ baseTool }

type mcpPoolView interface {
	AllTools() []mcp.ToolRef
	AllServers() []*mcp.ManagedServer
	CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (*mcp.CallToolResult, error)
	ReadResource(ctx context.Context, serverName, uri string) (*mcp.ReadResourceResult, error)
}

func NewMCPTool(pool mcpPoolView) Tool {
	return &mcpToolProxy{pool: pool, baseTool: baseTool{def: Def{
		Name: "mcp",
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

func (t *mcpToolProxy) Def() Def {
	d := t.def
	if t.pool == nil {
		return d
	}
	tools := t.pool.AllTools()
	for _, tr := range tools {
		d.Description += fmt.Sprintf("\n  [%s] %s: %s", tr.Server, tr.Tool.Name, tr.Tool.Description)
	}
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

	var sb strings.Builder
	if result.IsError {
		sb.WriteString("[MCP Error] ")
	}
	for _, c := range result.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		} else {
			sb.WriteString(fmt.Sprintf("[%s: %s]", c.Type, truncate(c.Data, 200)))
		}
	}
	return Result{Data: sb.String(), IsError: result.IsError}, nil
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
		Name: "mcp_resources",
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
		Name: "mcp_read_resource",
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
		sb.WriteString(fmt.Sprintf("[%s] %d tools, %d resources\n", s.Name, len(s.Tools), len(s.Resources)))
		for _, tool := range s.Tools {
			sb.WriteString(fmt.Sprintf("  tool: %s - %s\n", tool.Name, tool.Description))
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
		if block.Text != "" {
			sb.WriteString(block.Text)
			if !strings.HasSuffix(block.Text, "\n") {
				sb.WriteString("\n")
			}
			continue
		}
		if block.Data != "" {
			sb.WriteString(block.Data)
			if !strings.HasSuffix(block.Data, "\n") {
				sb.WriteString("\n")
			}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
