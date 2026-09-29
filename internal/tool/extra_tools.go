package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type WebSearchTool struct {
	baseTool
	settings WebSearchSettings
}

// WebSearchSettings is the web_search config key: Provider is "tavily",
// "brave" or "duckduckgo"; APIKey is the key for tavily/brave.
type WebSearchSettings struct {
	Provider string
	APIKey   string
}

// ResolveWebSearchBackend picks the backend websearch uses and its key. A
// configured provider wins; its key comes from the config or, when absent,
// from the provider's environment variable. Without a provider the
// environment decides (TAVILY_API_KEY, then BRAVE_API_KEY /
// BRAVE_SEARCH_API_KEY). A keyed provider with no key, and everything else,
// falls back to the key-less DuckDuckGo scraper.
func ResolveWebSearchBackend(cfg WebSearchSettings) (provider, apiKey string) {
	envKey := func(p string) string {
		switch p {
		case "tavily":
			return os.Getenv("TAVILY_API_KEY")
		case "brave":
			if k := os.Getenv("BRAVE_API_KEY"); k != "" {
				return k
			}
			return os.Getenv("BRAVE_SEARCH_API_KEY")
		}
		return ""
	}
	switch p := strings.ToLower(strings.TrimSpace(cfg.Provider)); p {
	case "tavily", "brave":
		key := strings.TrimSpace(cfg.APIKey)
		if key == "" {
			key = envKey(p)
		}
		if key == "" {
			return "duckduckgo", ""
		}
		return p, key
	case "duckduckgo", "ddg":
		return "duckduckgo", ""
	}
	for _, p := range []string{"tavily", "brave"} {
		if k := envKey(p); k != "" {
			return p, k
		}
	}
	return "duckduckgo", ""
}

type QuestionTool struct{ baseTool }
type TodoWriteTool struct{ baseTool }

var (
	webSearchEndpoint   = "https://html.duckduckgo.com/html/"
	webSearchHTTPClient = &http.Client{Timeout: 15 * time.Second}
	webSearchLinkRE     = regexp.MustCompile(`(?is)<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	webSearchTagRE      = regexp.MustCompile(`(?is)<[^>]+>`)
	webSearchSpaceRE    = regexp.MustCompile(`\s+`)
)

func NewWebSearchTool() Tool { return NewWebSearchToolWith(WebSearchSettings{}) }

// NewWebSearchToolWith is NewWebSearchTool using the web_search config key.
func NewWebSearchToolWith(settings WebSearchSettings) Tool {
	return &WebSearchTool{baseTool: baseTool{def: Def{
		Name: "websearch", Description: "Search the web and return live results.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		IsReadOnly:  true, IsConcurrencySafe: true, UserFacingName: "WebSearch",
	}}, settings: settings}
}
func (t *WebSearchTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	q, _ := input["query"].(string)
	q = strings.TrimSpace(q)
	if q == "" {
		return Result{Data: "Error: query required", IsError: true}, nil
	}

	// Tavily / Brave when configured (web_search or environment), else the
	// zero-config DuckDuckGo scraper below.
	switch provider, apiKey := ResolveWebSearchBackend(t.settings); provider {
	case "tavily":
		return t.queryTavily(ctx, apiKey, q)
	case "brave":
		return t.queryBrave(ctx, apiKey, q)
	}

	// 3. Zero-config Fallback: DuckDuckGo Scraper
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webSearchEndpoint+"?q="+urlQueryEscape(q), nil)
	if err != nil {
		return Result{Data: "WebSearch error: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "cove/1.0")

	resp, err := webSearchHTTPClient.Do(req)
	if err != nil {
		return Result{Data: "WebSearch error: " + err.Error(), IsError: true}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{Data: "WebSearch read error: " + err.Error(), IsError: true}, nil
	}

	results := extractWebSearchResults(string(body), 5)
	if len(results) == 0 {
		return Result{Data: fmt.Sprintf("WebSearch: %s\nNo live results found.", q)}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "WebSearch (DuckDuckGo Fallback): %s\n", q)
	for i, result := range results {
		fmt.Fprintf(&sb, "%d. %s\n   %s", i+1, result.title, result.url)
		if result.snippet != "" {
			fmt.Fprintf(&sb, "\n   %s", result.snippet)
		}
		sb.WriteString("\n")
	}
	return Result{Data: strings.TrimSpace(sb.String())}, nil
}

func (t *WebSearchTool) queryTavily(ctx context.Context, apiKey string, query string) (Result, error) {
	payload := map[string]any{
		"api_key":     apiKey,
		"query":       query,
		"max_results": 5,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return Result{Data: "Tavily JSON marshal error: " + err.Error(), IsError: true}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", strings.NewReader(string(bodyBytes)))
	if err != nil {
		return Result{Data: "Tavily build request error: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Data: "Tavily API call error: " + err.Error(), IsError: true}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return Result{Data: fmt.Sprintf("Tavily error response (status %d): %s", resp.StatusCode, string(respBody)), IsError: true}, nil
	}

	var res struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return Result{Data: "Tavily JSON decode error: " + err.Error(), IsError: true}, nil
	}

	if len(res.Results) == 0 {
		return Result{Data: fmt.Sprintf("WebSearch (Tavily): %s\nNo results found.", query)}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "WebSearch (Tavily Grounding): %s\n", query)
	for i, item := range res.Results {
		fmt.Fprintf(&sb, "%d. %s\n   %s", i+1, item.Title, item.URL)
		if item.Content != "" {
			fmt.Fprintf(&sb, "\n   %s", strings.TrimSpace(item.Content))
		}
		sb.WriteString("\n")
	}
	return Result{Data: strings.TrimSpace(sb.String())}, nil
}

func (t *WebSearchTool) queryBrave(ctx context.Context, apiKey string, query string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.search.brave.com/res/v1/web/search?q="+urlQueryEscape(query), nil)
	if err != nil {
		return Result{Data: "Brave build request error: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("X-Subscription-Token", apiKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Data: "Brave API call error: " + err.Error(), IsError: true}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return Result{Data: fmt.Sprintf("Brave error response (status %d): %s", resp.StatusCode, string(respBody)), IsError: true}, nil
	}

	var res struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return Result{Data: "Brave JSON decode error: " + err.Error(), IsError: true}, nil
	}

	if len(res.Web.Results) == 0 {
		return Result{Data: fmt.Sprintf("WebSearch (Brave): %s\nNo results found.", query)}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "WebSearch (Brave Grounding): %s\n", query)
	for i, item := range res.Web.Results {
		fmt.Fprintf(&sb, "%d. %s\n   %s", i+1, item.Title, item.URL)
		if item.Description != "" {
			fmt.Fprintf(&sb, "\n   %s", strings.TrimSpace(item.Description))
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
		IsConcurrencySafe: false, PlanSafe: true, UserFacingName: "Question",
	}}}
}

// AskUserCancelled is what Runtime.AskUser returns when the user interrupted
// the question (Ctrl+C) instead of answering it.
const AskUserCancelled = "\x00cancel"

// questionCancelled is the question tool's result for an interrupted ask.
var questionCancelled = Result{Data: "Error: cancelled by user", IsError: true}

func (t *QuestionTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if tctx.IsNonInteractive || tctx.Runtime == nil || tctx.Runtime.AskUser == nil {
		return Result{Data: "[Question requires interactive mode]", IsError: true}, nil
	}
	questions, _ := input["questions"].([]any)
	var sb strings.Builder
	for i, q := range questions {
		// A cancelled turn (Ctrl+C) asks nothing more: a later question
		// would take the user's next line as its answer.
		if ctx.Err() != nil {
			return questionCancelled, nil
		}
		qm, _ := q.(map[string]any)
		h, _ := qm["header"].(string)
		qt, _ := qm["question"].(string)
		var prompt strings.Builder
		fmt.Fprintf(&prompt, "[Q%d] %s\n%s\n", i+1, h, qt)
		opts, _ := qm["options"].([]any)
		labels := make([]string, 0, len(opts))
		for idx, o := range opts {
			om, _ := o.(map[string]any)
			label := fmt.Sprint(om["label"])
			labels = append(labels, label)
			fmt.Fprintf(&prompt, "  %d. %v: %v\n", idx+1, om["label"], om["description"])
		}
		if tctx.SetWaiting != nil {
			tctx.SetWaiting(true)
		}
		raw := tctx.Runtime.AskUser(prompt.String())
		if tctx.SetWaiting != nil {
			tctx.SetWaiting(false)
		}
		if raw == AskUserCancelled || ctx.Err() != nil {
			return questionCancelled, nil
		}
		answer := strings.TrimSpace(raw)
		selected := answer
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(labels) {
			selected = labels[n-1]
		}
		if selected == "" {
			selected = "(empty)"
		}
		fmt.Fprintf(&sb, "[Q%d] %s\nAnswer: %s\n\n", i+1, qt, selected)
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
			"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed","cancelled"]},"priority":{"type":"string","enum":["high","medium","low"]}
		},"required":["content","status","priority"]}}},"required":["todos"]}`),
		IsReadOnly: false, IsConcurrencySafe: false, PlanSafe: true, UserFacingName: "TodoWrite",
	}}}
}
func (t *TodoWriteTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	todos, _ := input["todos"].([]any)
	if tctx.Runtime != nil {
		// todowrite is not concurrency-safe, so the engine runs it inline — but
		// inline means "in the dispatch loop", concurrently with the tool calls
		// already launched as goroutines from that same response. SetTodos
		// takes the lock.
		tctx.Runtime.SetTodos(todos)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task list (%d items):\n", len(todos))
	for i, td := range todos {
		tm, _ := td.(map[string]any)
		status, _ := tm["status"].(string)
		content, _ := tm["content"].(string)
		priority, _ := tm["priority"].(string)
		writeTodoLine(&sb, i+1, status, content, priority)
	}
	return Result{Data: sb.String()}, nil
}

// writeTodoLine renders one todo as todowrite shows it. The ID is what a
// later todo names in "depends:todo-N", so it is shown.
func writeTodoLine(sb *strings.Builder, n int, status, content, priority string) {
	mark := "[ ]"
	switch status {
	case "completed":
		mark = "[✓]"
	case "in_progress":
		mark = "[>]"
	case "cancelled":
		mark = "[x]"
	}
	fmt.Fprintf(sb, "%s todo-%d. %s [%s]\n", mark, n, content, priority)
}

// SetTodos replaces the todo list (the todo-N task records) with todos, the
// "todos" argument of a todowrite call. The engine also calls it on resume,
// with the last todowrite call of the loaded history.
func (r *Runtime) SetTodos(todos []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Tasks == nil {
		r.Tasks = make(map[string]*TaskRecord)
	}
	r.clearTodosLocked()
	for i, td := range todos {
		tm, _ := td.(map[string]any)
		content, _ := tm["content"].(string)
		status, _ := tm["status"].(string)
		priority, _ := tm["priority"].(string)
		id := fmt.Sprintf("todo-%d", i+1)
		// Description is what execute_plan sends a sub-agent as the task and
		// where it parses the "depends:" prefix. It used to hold only
		// "priority: high", so every sub-agent got that as its whole task.
		r.Tasks[id] = &TaskRecord{
			ID:          id,
			Title:       content,
			Description: content,
			Status:      status,
			Priority:    priority,
		}
	}
}

// ClearTodos drops the todo list (a new conversation starts without one).
func (r *Runtime) ClearTodos() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearTodosLocked()
}

func (r *Runtime) clearTodosLocked() {
	for id := range r.Tasks {
		if strings.HasPrefix(id, "todo-") {
			delete(r.Tasks, id)
		}
	}
}

// CompletedTodos returns the content of the todo items marked completed.
func (r *Runtime) CompletedTodos() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for id, tr := range r.Tasks {
		if strings.HasPrefix(id, "todo-") && tr.Status == "completed" {
			out = append(out, tr.Title)
		}
	}
	sort.Strings(out)
	return out
}

// TodoList renders the current todo list as todowrite shows it and counts
// the items still open (pending or in progress). Empty with no list.
func (r *Runtime) TodoList() (list string, open int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	type item struct {
		n  int
		tr *TaskRecord
	}
	var items []item
	for id, tr := range r.Tasks {
		n, err := strconv.Atoi(strings.TrimPrefix(id, "todo-"))
		if err != nil || !strings.HasPrefix(id, "todo-") {
			continue
		}
		items = append(items, item{n, tr})
	}
	if len(items) == 0 {
		return "", 0
	}
	sort.Slice(items, func(i, j int) bool { return items[i].n < items[j].n })
	var sb strings.Builder
	for _, it := range items {
		writeTodoLine(&sb, it.n, it.tr.Status, it.tr.Title, it.tr.Priority)
		if it.tr.Status == "pending" || it.tr.Status == "in_progress" {
			open++
		}
	}
	return sb.String(), open
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
