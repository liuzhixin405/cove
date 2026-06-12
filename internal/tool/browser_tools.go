package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/browser"
)

// BrowserTool drives a (optionally headless-Chrome) browser to fetch
// JavaScript-rendered content or capture screenshots. When the binary is built
// without the "chromedp" tag, the navigate/read actions transparently fall back
// to HTTP fetch and screenshot reports that headless mode is unavailable.
type BrowserTool struct {
	baseTool
	br *browser.Browser
}

func NewBrowserTool() Tool {
	return &BrowserTool{
		baseTool: baseTool{def: Def{
			Name:        "browser",
			Description: "Drive a headless browser. action=navigate renders a page (executing JavaScript) and returns text/markdown/html; action=screenshot saves a PNG of the page. Falls back to HTTP fetch when headless Chrome is unavailable.",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"action":{"type":"string","enum":["navigate","screenshot"],"description":"navigate to read rendered content, or screenshot to capture a PNG"},
					"url":{"type":"string","description":"The URL to open"},
					"format":{"type":"string","enum":["text","markdown","html"],"description":"navigate output format (default text)"},
					"output":{"type":"string","description":"screenshot file path (default browser-screenshot.png)"}
				},
				"required":["action","url"]
			}`),
			IsReadOnly: true, IsConcurrencySafe: false, UserFacingName: "Browser",
		}},
		br: browser.New(browser.DefaultConfig()),
	}
}

func (t *BrowserTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	action, _ := input["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	rawURL, _ := input["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return Result{Data: "Error: url required", IsError: true}, nil
	}

	switch action {
	case "", "navigate", "open", "read":
		format, _ := input["format"].(string)
		format = strings.ToLower(strings.TrimSpace(format))
		if format == "" {
			format = "text"
		}
		if format != "text" && format != "markdown" && format != "html" {
			return Result{Data: "Error: format must be one of text, markdown, html", IsError: true}, nil
		}
		var (
			res *browser.FetchResult
			err error
		)
		if t.br.ChromeAvailable() {
			res, err = t.br.FetchRendered(ctx, rawURL, format)
		} else {
			res, err = t.br.FetchMarkdown(ctx, rawURL)
			if err == nil && format != "markdown" {
				// re-fetch in requested format via HTTP path
				switch format {
				case "text":
					res, err = t.br.FetchHeadless(ctx, rawURL)
				case "html":
					res, err = t.br.FetchHTML(ctx, rawURL)
				}
			}
		}
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		mode := "headless-chrome"
		if !t.br.ChromeAvailable() {
			mode = "http"
		}
		header := fmt.Sprintf("URL: %s\nStatus: %d\nFormat: %s\nMode: %s\n\n", res.URL, res.StatusCode, res.Format, mode)
		return Result{Data: header + res.Content}, nil

	case "screenshot", "capture":
		if !t.br.ChromeAvailable() {
			return Result{Data: "Error: " + browser.ErrChromeUnavailable.Error(), IsError: true}, nil
		}
		out, _ := input["output"].(string)
		out = strings.TrimSpace(out)
		if out == "" {
			out = "browser-screenshot.png"
		}
		if !filepath.IsAbs(out) && tctx.Cwd != "" {
			out = filepath.Join(tctx.Cwd, out)
		}
		png, err := t.br.Screenshot(ctx, rawURL)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		if err := os.WriteFile(out, png, 0644); err != nil {
			return Result{Data: "Error writing screenshot: " + err.Error(), IsError: true}, nil
		}
		return Result{Data: fmt.Sprintf("Saved screenshot (%d bytes) to %s", len(png), out)}, nil

	default:
		return Result{Data: "Error: unknown action " + action + " (use navigate or screenshot)", IsError: true}, nil
	}
}

func (t *BrowserTool) Validate(input Input) string {
	if _, ok := input["url"].(string); !ok {
		return "url is required"
	}
	return ""
}

func (t *BrowserTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	u, _ := input["url"].(string)
	if isPrivateURL(u) {
		return Denied("access to private/internal URLs is blocked")
	}
	action, _ := input["action"].(string)
	if strings.EqualFold(strings.TrimSpace(action), "screenshot") {
		// Writes a file to disk; surface for confirmation under default mode.
		return Asked("browser screenshot writes a PNG file")
	}
	return Allowed("browser navigation is read-only")
}
