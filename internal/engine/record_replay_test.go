package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

func TestRecordReplayHooksCreateArtifacts(t *testing.T) {
	tmpDir := t.TempDir()
	recordDir := filepath.Join(tmpDir, "recordings")

	eng := &Engine{}
	eng.recordingEnabled = true
	eng.recordingDir = recordDir
	eng.recordingSeq = 0

	ctx := context.Background()
	eng.recordEvent(ctx, "request", map[string]any{"model": "test-model"})
	eng.recordEvent(ctx, "response", map[string]any{"status": "ok"})

	if _, err := os.Stat(filepath.Join(recordDir, "events.jsonl")); err != nil {
		t.Fatalf("expected events file to exist: %v", err)
	}

	if _, err := os.Stat(filepath.Join(recordDir, "meta.json")); err != nil {
		t.Fatalf("expected meta file to exist: %v", err)
	}
}

func TestLoadReplayResponsesAndConsume(t *testing.T) {
	tmpDir := t.TempDir()
	replayDir := filepath.Join(tmpDir, "session_001")
	if err := os.MkdirAll(replayDir, 0o755); err != nil {
		t.Fatalf("mkdir replay dir: %v", err)
	}

	line1 := `{"event":"llm_response","payload":{"response":{"Content":"step1","StopReason":"tool_use","ToolCalls":[{"id":"1","name":"read","input":{"filePath":"README.md"}}]}}}`
	line2 := `{"event":"llm_response","payload":{"response":{"Content":"done","StopReason":"stop"}}}`
	payload := fmt.Sprintf("%s\n%s\n", line1, line2)
	if err := os.WriteFile(filepath.Join(replayDir, "events.jsonl"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write replay file: %v", err)
	}

	eng := &Engine{replayEnabled: true, replayDir: replayDir}
	if err := eng.loadReplayResponses(); err != nil {
		t.Fatalf("load replay responses: %v", err)
	}

	resp1, err := eng.nextReplayResponse(false, nil, nil)
	if err != nil {
		t.Fatalf("first replay response: %v", err)
	}
	if resp1.Content != "step1" {
		t.Fatalf("expected first replay content step1, got %q", resp1.Content)
	}
	if len(resp1.ToolCalls) != 1 || resp1.ToolCalls[0].Name != "read" {
		t.Fatalf("unexpected tool calls: %+v", resp1.ToolCalls)
	}

	gotDelta := ""
	resp2, err := eng.nextReplayResponse(true, func(delta string) { gotDelta += delta }, nil)
	if err != nil {
		t.Fatalf("second replay response: %v", err)
	}
	if resp2.Content != "done" {
		t.Fatalf("expected second replay content done, got %q", resp2.Content)
	}
	if gotDelta != "done" {
		t.Fatalf("expected streamed delta done, got %q", gotDelta)
	}
}

func TestEnableRecordingWritesMeta(t *testing.T) {
	tmpDir := t.TempDir()
	eng := &Engine{}
	if err := eng.EnableRecording(tmpDir); err != nil {
		t.Fatalf("enable recording: %v", err)
	}
	defer eng.DisableRecording()

	enabled, dir := eng.RecordingStatus()
	if !enabled {
		t.Fatal("expected recording to be enabled")
	}
	if dir != tmpDir {
		t.Fatalf("expected recording dir %q, got %q", tmpDir, dir)
	}
	if !eng.recordingReady {
		t.Fatal("expected recording to be marked ready after EnableRecording")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "meta.json")); err != nil {
		t.Fatalf("expected meta file to exist: %v", err)
	}

	eng.recordEvent(context.Background(), "llm_response", map[string]any{
		"response": api.ChatResponse{Content: "ok"},
	})
	if _, err := os.Stat(filepath.Join(tmpDir, "events.jsonl")); err != nil {
		t.Fatalf("expected events file to exist: %v", err)
	}
}

func TestDisableRecordingResetsReadyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	eng := &Engine{}
	if err := eng.EnableRecording(tmpDir); err != nil {
		t.Fatalf("enable recording: %v", err)
	}
	eng.DisableRecording()
	if eng.recordingReady {
		t.Fatal("expected recordingReady=false after DisableRecording")
	}
}
