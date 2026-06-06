package mcp

import "encoding/json"

type JSONRPC struct {
	Jsonrpc string `json:"jsonrpc"`
}

type Request struct {
	JSONRPC
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

type Notification struct {
	JSONRPC
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    ClientCaps     `json:"capabilities"`
	ClientInfo      Implementation `json:"clientInfo"`
}

type ClientCaps struct {
	Roots    *RootsCaps `json:"roots,omitempty"`
	Sampling *struct{}  `json:"sampling,omitempty"`
}

type RootsCaps struct {
	ListChanged bool `json:"listChanged"`
}

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    ServerCaps     `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
}

type ServerCaps struct {
	Tools     *ToolsCaps     `json:"tools,omitempty"`
	Resources *ResourcesCaps `json:"resources,omitempty"`
	Prompts   *PromptsCaps   `json:"prompts,omitempty"`
}

type ToolsCaps struct {
	ListChanged bool `json:"listChanged"`
}

type ResourcesCaps struct {
	Subscribe   bool `json:"subscribe"`
	ListChanged bool `json:"listChanged"`
}

type PromptsCaps struct {
	ListChanged bool `json:"listChanged"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type CallToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type ListResourcesResult struct {
	Resources []Resource `json:"resources"`
}

type ReadResourceParams struct {
	URI string `json:"uri"`
}

type ReadResourceResult struct {
	Contents []ContentBlock `json:"contents"`
}

type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type ListPromptsResult struct {
	Prompts []Prompt `json:"prompts"`
}

type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
}
