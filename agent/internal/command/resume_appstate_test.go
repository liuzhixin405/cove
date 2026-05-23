package command

import (
	"context"
	"testing"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/session"
	"github.com/agentgo/internal/state"
)

func TestResumeCmdUpdatesAppState(t *testing.T) {
	store := &fakeSessionStore{records: map[string]session.Record{
		"resume-1": {ID: "resume-1", Title: "test", Model: "gpt-4o", Messages: []api.Message{{Role: "user", Content: "resume me"}, {Role: "assistant", Content: "done"}}},
	}}
	eng := &fakeEngine{}
	appState := &state.AppState{}
	cmd := NewResumeCmd()

	_, err := cmd.Execute(context.Background(), Input{Args: []string{"resume-1"}, SessionStore: store, Engine: eng, AppState: appState})
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if appState.SessionID != "resume-1" {
		t.Fatalf("expected app state session id updated, got %q", appState.SessionID)
	}
	if appState.Model != "gpt-4o" {
		t.Fatalf("expected app state model updated, got %q", appState.Model)
	}
	if appState.Messages != 2 {
		t.Fatalf("expected app state messages updated, got %d", appState.Messages)
	}
}