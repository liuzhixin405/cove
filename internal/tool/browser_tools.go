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
			// One request in the requested format. This used to fetch as
			// markdown first and then fetch again for text/html, doubling the
			// latency and any side effect of the GET.
			switch format {
			case "markdown":
				res, err = t.br.FetchMarkdown(ctx, rawURL)
			case "html":
				res, err = t.br.FetchHTML(ctx, rawURL)
			default:
				res, err = t.br.FetchHeadless(ctx, rawURL)
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
		out, err := screenshotPath(input, tctx.Cwd)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		if !t.br.ChromeAvailable() {
			return Result{Data: "Error: " + browser.ErrChromeUnavailable.Error(), IsError: true}, nil
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
	if isScreenshotAction(input) {
		out, err := screenshotPath(input, tctx.Cwd)
		if err != nil {
			return Denied(err.Error())
		}
		// Writes a file to disk; surface for confirmation under default mode.
		return Asked("browser screenshot writes a PNG file: " + out)
	}
	return Allowed("browser navigation is read-only")
}

// isScreenshotAction reports whether the call writes a screenshot. It must
// accept every alias Call does: CheckPermissions used to match only the literal
// "screenshot", so action=capture wrote its file without asking.
func isScreenshotAction(input Input) bool {
	action, _ := input["action"].(string)
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "screenshot", "capture":
		return true
	}
	return false
}

// screenshotPath resolves the screenshot's output path and refuses anything
// but a .png file inside the workspace. The tool is marked read-only, so a
// read-only sub-agent runs it with no permission gate at all, and the path is
// picked by the model: without this check an absolute path or "../" let such a
// call overwrite any file the user can write.
func screenshotPath(input Input, cwd string) (string, error) {
	out, _ := input["output"].(string)
	out = strings.TrimSpace(out)
	if out == "" {
		out = "browser-screenshot.png"
	}
	if !strings.EqualFold(filepath.Ext(out), ".png") {
		return "", fmt.Errorf("screenshot output must be a .png file, got %q", out)
	}
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("screenshot output: cannot determine the workspace: %w", err)
		}
		cwd = wd
	}
	abs := out
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(filepath.Clean(cwd), abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("screenshot output must be inside the workspace %s, got %q", cwd, out)
	}
	return abs, nil
}
