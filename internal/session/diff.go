package session

import (
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
)

// SessionView is a lightweight snapshot of a conversation at a point in time.
type SessionView struct {
	Messages []api.Message
	Tokens   int
}

// NewSessionView creates a session snapshot.
func NewSessionView(messages []api.Message, tokens int) *SessionView {
	msgs := make([]api.Message, len(messages))
	copy(msgs, messages)
	return &SessionView{Messages: msgs, Tokens: tokens}
}

// MessagesCount returns the number of messages in the view.
func (sv *SessionView) MessagesCount() int { return len(sv.Messages) }

// SessionDiff captures changes between two session views.
type SessionDiff struct {
	OldTokens    int
	NewTokens    int
	OldMsgCount  int
	NewMsgCount  int
	AddedTools   []string
	RemovedTools []string
	AddedFiles   []string
	RemovedFiles []string
}

// HasChanges returns true if any difference exists.
func (sd *SessionDiff) HasChanges() bool {
	return sd.OldTokens != sd.NewTokens ||
		sd.OldMsgCount != sd.NewMsgCount ||
		len(sd.AddedTools) > 0 ||
		len(sd.RemovedTools) > 0 ||
		len(sd.AddedFiles) > 0 ||
		len(sd.RemovedFiles) > 0
}

// Summary returns a human-readable summary of changes.
func (sd *SessionDiff) Summary() string {
	if !sd.HasChanges() {
		return "no changes"
	}
	var parts []string
	if sd.OldMsgCount != sd.NewMsgCount {
		parts = append(parts, fmt.Sprintf("messages: %d → %d", sd.OldMsgCount, sd.NewMsgCount))
	}
	if sd.OldTokens != sd.NewTokens {
		parts = append(parts, fmt.Sprintf("tokens: %d → %d", sd.OldTokens, sd.NewTokens))
	}
	if len(sd.AddedTools) > 0 {
		parts = append(parts, fmt.Sprintf("tools +%v", sd.AddedTools))
	}
	if len(sd.AddedFiles) > 0 {
		parts = append(parts, fmt.Sprintf("files +%v", sd.AddedFiles))
	}
	return strings.Join(parts, "; ")
}

// Diff computes the difference between two session views.
func Diff(a, b *SessionView) *SessionDiff {
	if a == nil || b == nil {
		return &SessionDiff{}
	}

	d := &SessionDiff{
		OldTokens:   a.Tokens,
		NewTokens:   b.Tokens,
		OldMsgCount: len(a.Messages),
		NewMsgCount: len(b.Messages),
	}

	// Extract tools and files from both views
	aTools, aFiles := extractArtifacts(a.Messages)
	bTools, bFiles := extractArtifacts(b.Messages)

	d.AddedTools = diffStrings(aTools, bTools)
	d.RemovedTools = diffStrings(bTools, aTools)
	d.AddedFiles = diffStrings(aFiles, bFiles)
	d.RemovedFiles = diffStrings(bFiles, aFiles)

	return d
}

// extractArtifacts extracts unique tool names and file paths from messages.
func extractArtifacts(messages []api.Message) (tools []string, files []string) {
	toolSet := make(map[string]bool)
	fileSet := make(map[string]bool)

	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			toolSet[tc.Name] = true
			// Extract file paths from common parameter names
			for _, key := range []string{"filePath", "path", "target", "dest", "output"} {
				if v, ok := tc.Input[key].(string); ok && v != "" {
					fileSet[v] = true
				}
			}
		}
	}

	for t := range toolSet {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)
	return
}

// diffStrings returns strings in a that are not in b.
func diffStrings(a, b []string) []string {
	bSet := make(map[string]bool, len(b))
	for _, s := range b {
		bSet[s] = true
	}
	var result []string
	for _, s := range a {
		if !bSet[s] {
			result = append(result, s)
		}
	}
	return result
}
