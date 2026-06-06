package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type WebSearchTool struct{ baseTool }
type QuestionTool struct{ baseTool }
type TodoWriteTool struct{ baseTool }

var (
	webSearchEndpoint   = "https://html.duckduckgo.com/html/"
	webSearchHTTPClient = &http.Client{Timeout: 15 * time.Second}
	webSearchLinkRE     = regexp.MustCompile(`(?is)<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	webSearchTagRE      = regexp.MustCompile(`(?is)<[^>]+>`)
	webSearchSpaceRE    = regexp.MustCompile(`\s+`)
)

func NewWebSearchTool() Tool {
	return &WebSearchTool{baseTool{def: Def{
		Name: "websearch", Description: "Search the web and return live results.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		IsReadOnly:  true, IsConcurrencySafe: true, UserFacingName: "WebSearch",
	}}}
}
func (t *WebSearchTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	q, _ := input["query"].(string)
	q = strings.TrimSpace(q)
	if q == "" {
		return Result{Data: "Error: query required", IsError: true}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webSearchEndpoint+"?q="+urlQueryEscape(q), nil)
	if err != nil {
		return Result{Data: "WebSearch error: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "cove/1.0")

	resp, err := webSearchHTTPClient.Do(req)
	if err != nil {
		return Result{Data: "WebSearch error: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{Data: "WebSearch read error: " + err.Error(), IsError: true}, nil
	}

	results := extractWebSearchResults(string(body), 5)
	if len(results) == 0 {
		return Result{Data: fmt.Sprintf("WebSearch: %s\nNo live results found.", q)}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("WebSearch: %s\n", q))
	for i, result := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s", i+1, result.title, result.url))
		if result.snippet != "" {
			sb.WriteString(fmt.Sprintf("\n   %s", result.snippet))
		}
		sb.WriteString("\n")
	}
	return Result{Data: strings.TrimSpace(sb.String())}, nil
}
func (t *WebSearchTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("websearch is read-only")
}

func NewQuestionTool() Tool {
	return &QuestionTool{baseTool{def: Def{
		Name: "question", Aliases: []string{"Question"},
		Description: "Ask the user multiple-choice questions for clarification or preferences.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{
			"question":{"type":"string"},"header":{"type":"string"},
			"options":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"description":{"type":"string"}}}},
			"multiple":{"type":"boolean"}
		}}},"required":["questions"]}`),
		IsConcurrencySafe: false, UserFacingName: "Question",
	}}}
}
func (t *QuestionTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if tctx.IsNonInteractive || tctx.Runtime == nil || tctx.Runtime.AskUser == nil {
		return Result{Data: "[Question requires interactive mode]", IsError: true}, nil
	}
	questions, _ := input["questions"].([]any)
	var sb strings.Builder
	for i, q := range questions {
		qm, _ := q.(map[string]any)
		h, _ := qm["header"].(string)
		qt, _ := qm["question"].(string)
		var prompt strings.Builder
		prompt.WriteString(fmt.Sprintf("[Q%d] %s\n%s\n", i+1, h, qt))
		opts, _ := qm["options"].([]any)
		labels := make([]string, 0, len(opts))
		for idx, o := range opts {
			om, _ := o.(map[string]any)
			label := fmt.Sprint(om["label"])
			labels = append(labels, label)
			prompt.WriteString(fmt.Sprintf("  %d. %v: %v\n", idx+1, om["label"], om["description"]))
		}
		answer := strings.TrimSpace(tctx.Runtime.AskUser(prompt.String()))
		selected := answer
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(labels) {
			selected = labels[n-1]
		}
		if selected == "" {
			selected = "(empty)"
		}
		sb.WriteString(fmt.Sprintf("[Q%d] %s\nAnswer: %s\n\n", i+1, qt, selected))
	}
	return Result{Data: strings.TrimSpace(sb.String())}, nil
}
func (t *QuestionTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("question is interactive")
}

func NewTodoWriteTool() Tool {
	return &TodoWriteTool{baseTool{def: Def{
		Name: "todowrite", Aliases: []string{"TodoWrite"},
		Description: "Create and manage a structured task list. Track progress of multi-step tasks.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{
			"content":{"type":"string"},"status":{"type":"string"},"priority":{"type":"string"}
		},"required":["content","status","priority"]}}},"required":["todos"]}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "TodoWrite",
	}}}
}
func (t *TodoWriteTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	todos, _ := input["todos"].([]any)
	if tctx.Runtime != nil {
		if tctx.Runtime.Tasks == nil {
			tctx.Runtime.Tasks = make(map[string]*TaskRecord)
		}
		for id := range tctx.Runtime.Tasks {
			if strings.HasPrefix(id, "todo-") {
				delete(tctx.Runtime.Tasks, id)
			}
		}
		for i, td := range todos {
			tm, _ := td.(map[string]any)
			content, _ := tm["content"].(string)
			status, _ := tm["status"].(string)
			priority, _ := tm["priority"].(string)
			id := fmt.Sprintf("todo-%d", i+1)
			tctx.Runtime.Tasks[id] = &TaskRecord{
				ID:          id,
				Title:       content,
				Description: fmt.Sprintf("priority: %s", priority),
				Status:      status,
			}
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task list (%d items):\n", len(todos)))
	for i, td := range todos {
		tm, _ := td.(map[string]any)
		status, _ := tm["status"].(string)
		mark := "[ ]"
		switch status {
		case "completed":
			mark = "[✓]"
		case "in_progress":
			mark = "[>]"
		case "cancelled":
			mark = "[x]"
		}
		sb.WriteString(fmt.Sprintf("%s %d. %v [%v]\n", mark, i+1, tm["content"], tm["priority"]))
	}
	return Result{Data: sb.String()}, nil
}
func (t *TodoWriteTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("todowrite is local state")
}

type webSearchResult struct {
	title   string
	url     string
	snippet string
}

func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}

func extractWebSearchResults(body string, limit int) []webSearchResult {
	matches := webSearchLinkRE.FindAllStringSubmatch(body, -1)
	results := make([]webSearchResult, 0, limit)
	seen := map[string]struct{}{}
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		link := strings.TrimSpace(html.UnescapeString(match[1]))
		if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
			continue
		}
		if _, ok := seen[link]; ok {
			continue
		}
		title := cleanWebSearchText(match[2])
		if title == "" {
			continue
		}
		seen[link] = struct{}{}
		results = append(results, webSearchResult{
			title:   title,
			url:     link,
			snippet: extractSnippetNearLink(body, match[0]),
		})
		if len(results) >= limit {
			break
		}
	}
	return results
}

func extractSnippetNearLink(body, linkHTML string) string {
	idx := strings.Index(body, linkHTML)
	if idx < 0 {
		return ""
	}
	windowEnd := idx + len(linkHTML) + 400
	if windowEnd > len(body) {
		windowEnd = len(body)
	}
	window := body[idx+len(linkHTML) : windowEnd]
	for _, tag := range []string{"</a>", "<a "} {
		window = strings.ReplaceAll(window, tag, " ")
	}
	return cleanWebSearchText(window)
}

func cleanWebSearchText(s string) string {
	s = html.UnescapeString(s)
	s = webSearchTagRE.ReplaceAllString(s, " ")
	s = webSearchSpaceRE.ReplaceAllString(strings.TrimSpace(s), " ")
	return s
}
