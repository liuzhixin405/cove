package tool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type WebFetchTool struct{ baseTool }

func NewWebFetchTool() Tool {
	return &WebFetchTool{baseTool{def: Def{
		Name: "webfetch", Description: "Fetch content from a URL. Returns the page content. HTTP upgraded to HTTPS.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"url":{"type":"string","description":"The URL to fetch"},
				"format":{"type":"string","description":"Return format: text, markdown, or html"}
			},
			"required":["url"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "WebFetch",
	}}}
}

func (t *WebFetchTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	url, _ := input["url"].(string)
	if url == "" {
		return Result{Data: "Error: url required", IsError: true}, nil
	}

	if !strings.HasPrefix(url, "http") {
		url = "https://" + url
	}
	if strings.HasPrefix(url, "http://") {
		url = "https://" + strings.TrimPrefix(url, "http://")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "agentgo/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return Result{Data: "Error fetching: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return Result{Data: "Error reading: " + err.Error(), IsError: true}, nil
	}

	content := string(body)
	limit := 100000
	if len(content) > limit {
		content = content[:limit] + "\n... [truncated from " + itoa(len(body)) + " bytes]"
	}

	return Result{Data: "URL: " + url + "\nStatus: " + itoa(resp.StatusCode) + "\n\n" + strings.TrimSpace(content)}, nil
}

func (t *WebFetchTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("webfetch is read-only")
}
