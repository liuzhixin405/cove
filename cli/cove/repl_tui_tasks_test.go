package main

import (
	"strings"
	"testing"
)

func TestFormatTaskSnapshotText_EmptyQueue(t *testing.T) {
	got := formatTaskSnapshotText(false, nil)
	if !strings.Contains(got, "运行中: 否") {
		t.Fatalf("expected non-running state, got %q", got)
	}
	if !strings.Contains(got, "排队: 0") {
		t.Fatalf("expected empty queue, got %q", got)
	}
}

func TestFormatTaskSnapshotText_WithQueue(t *testing.T) {
	got := formatTaskSnapshotText(true, []string{"/review", "修复测试", "/context"})
	if !strings.Contains(got, "运行中: 是") {
		t.Fatalf("expected running state, got %q", got)
	}
	if !strings.Contains(got, "排队: 3") {
		t.Fatalf("expected queue length, got %q", got)
	}
	if !strings.Contains(got, "1. /review") || !strings.Contains(got, "2. 修复测试") {
		t.Fatalf("expected queue entries, got %q", got)
	}
}
