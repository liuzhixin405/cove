package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildUserMessageParsesInlineAttachments(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(filePath, []byte("hello attachment"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	msg, err := buildUserMessage("请总结 @note.txt", dir, nil)
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	if got, want := msg.Content, "请总结"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Parts))
	}
	if msg.Parts[0].Type != "text" {
		t.Fatalf("part type = %q, want text", msg.Parts[0].Type)
	}
	if !strings.Contains(msg.Parts[0].Text, "hello attachment") {
		t.Fatalf("text part missing file content: %q", msg.Parts[0].Text)
	}
}

func TestBuildUserMessageBuildsImagePart(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.png")
	pngData := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R'}
	if err := os.WriteFile(imgPath, pngData, 0o600); err != nil {
		t.Fatalf("write png: %v", err)
	}

	msg, err := buildUserMessage("看图", dir, []string{imgPath})
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Parts))
	}
	part := msg.Parts[0]
	if part.Type != "image" {
		t.Fatalf("part type = %q, want image", part.Type)
	}
	if part.MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", part.MimeType)
	}
	if got := part.Data; got != base64.StdEncoding.EncodeToString(pngData) {
		t.Fatalf("unexpected image base64 payload")
	}
}

func TestBuildUserMessageReturnsErrorForMissingAttachment(t *testing.T) {
	_, err := buildUserMessage("分析 @missing.txt", t.TempDir(), nil)
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestSplitQuotedFieldsSupportsSpacesInPaths(t *testing.T) {
	got, err := splitQuotedFields(`"screen shot.png" logs/app.log 'notes final.txt'`)
	if err != nil {
		t.Fatalf("splitQuotedFields returned error: %v", err)
	}
	want := []string{"screen shot.png", "logs/app.log", "notes final.txt"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
}

func TestAddAttachmentsNormalizesAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	var attached []string
	addAttachments([]string{"note.txt", "@note.txt"}, dir, &attached)
	if len(attached) != 1 {
		t.Fatalf("attached = %#v, want one normalized path", attached)
	}
	if attached[0] != filePath {
		t.Fatalf("attached path = %q, want %q", attached[0], filePath)
	}
}
