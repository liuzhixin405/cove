package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuestionToolNonInteractive(t *testing.T) {
	qt := NewQuestionTool()
	result, _ := qt.Call(context.Background(), Input{
		"questions": []any{map[string]any{"header": "Need input", "question": "Pick one"}},
	}, Context{IsNonInteractive: true})
	if !result.IsError {
		t.Fatalf("expected non-interactive mode to error, got %+v", result)
	}
}

func TestWebSearchToolReturnsLiveResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "golang agent" {
			t.Fatalf("expected query to be forwarded, got %q", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>
			<div class="result"><a href="https://example.com/a">Go Agent Guide</a><p>Build autonomous agents in Go.</p></div>
			<div class="result"><a href="https://example.com/b">Hermes Runtime</a><p>Runtime wiring notes.</p></div>
		</body></html>`))
	}))
	defer server.Close()

	oldEndpoint := webSearchEndpoint
	oldClient := webSearchHTTPClient
	webSearchEndpoint = server.URL
	webSearchHTTPClient = server.Client()
	defer func() {
		webSearchEndpoint = oldEndpoint
		webSearchHTTPClient = oldClient
	}()

	tw := NewWebSearchTool()
	result, _ := tw.Call(context.Background(), Input{"query": "golang agent"}, Context{})
	if result.IsError {
		t.Fatalf("websearch failed: %s", result.Data)
	}
	if strings.Contains(result.Data, "Configure SEARCH_API_KEY") {
		t.Fatalf("expected live search results, got placeholder %q", result.Data)
	}
	if !strings.Contains(result.Data, "Go Agent Guide") || !strings.Contains(result.Data, "https://example.com/a") {
		t.Fatalf("expected first live search result, got %q", result.Data)
	}
	if !strings.Contains(result.Data, "Hermes Runtime") {
		t.Fatalf("expected second live search result, got %q", result.Data)
	}
}

func TestTodoWriteToolMirrorsTodosIntoRuntime(t *testing.T) {
	tw := NewTodoWriteTool()
	rt := &Runtime{Tasks: map[string]*TaskRecord{
		"task-9":  {ID: "task-9", Title: "keep", Status: "running"},
		"todo-99": {ID: "todo-99", Title: "old todo", Status: "pending"},
	}}

	result, _ := tw.Call(context.Background(), Input{"todos": []any{
		map[string]any{"content": "wire runtime", "status": "in_progress", "priority": "high"},
		map[string]any{"content": "run tests", "status": "pending", "priority": "medium"},
	}}, Context{Runtime: rt})
	if result.IsError {
		t.Fatalf("todowrite failed: %s", result.Data)
	}
	if _, ok := rt.Tasks["todo-99"]; ok {
		t.Fatalf("expected stale todo entries to be replaced, runtime=%#v", rt.Tasks)
	}
	if len(rt.Tasks) != 3 {
		t.Fatalf("expected unrelated task plus two todos, got %d entries", len(rt.Tasks))
	}
	if got := rt.Tasks["todo-1"]; got == nil || got.Title != "wire runtime" || got.Status != "in_progress" || !strings.Contains(got.Description, "priority: high") {
		t.Fatalf("expected first todo mirrored into runtime, got %#v", got)
	}
	if got := rt.Tasks["todo-2"]; got == nil || got.Title != "run tests" || got.Status != "pending" || !strings.Contains(got.Description, "priority: medium") {
		t.Fatalf("expected second todo mirrored into runtime, got %#v", got)
	}
	if !strings.Contains(result.Data, "wire runtime") || !strings.Contains(result.Data, "run tests") {
		t.Fatalf("expected todo summary in response, got %q", result.Data)
	}
}
