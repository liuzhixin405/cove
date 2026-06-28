package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
)

// MaskingResult holds metrics about an output masking operation.
type MaskingResult struct {
	NewHistory  []api.Message
	MaskedCount int
	TokensSaved int
}

// ToolOutputMasker implements the Hybrid Backward Scanned FIFO algorithm
// to prevent large tool outputs from consuming the context window.
// Recent outputs (within protectionThreshold) are preserved; older ones
// are written to disk and replaced with placeholder markers.
type ToolOutputMasker struct {
	enabled              bool
	protectionThreshold  int             // tokens to protect from end (default 50000)
	minPrunableThreshold int             // min prunable before masking triggers (default 30000)
	outputDir            string          // ~/.cove/tool-outputs/
	exemptTools          map[string]bool // tools whose output is never masked
}

// NewToolOutputMasker creates a masker with sensible defaults.
func NewToolOutputMasker() *ToolOutputMasker {
	home, _ := os.UserHomeDir()
	return &ToolOutputMasker{
		enabled:              true,
		protectionThreshold:  50000,
		minPrunableThreshold: 30000,
		outputDir:            filepath.Join(home, ".cove", "tool-outputs"),
		exemptTools: map[string]bool{
			"question":       true,
			"todowrite":      true,
			"plan_mode":      true,
			"exit_plan_mode": true,
		},
	}
}

// Disable turns off masking.
func (m *ToolOutputMasker) Disable() { m.enabled = false }

// Mask runs the Hybrid Backward Scanned FIFO algorithm on the message history.
// It scans from the end, protects the most recent ~protectionThreshold tokens,
// then masks older tool outputs that exceed minPrunableThreshold.
func (m *ToolOutputMasker) Mask(history []api.Message, toolNames []string) (*MaskingResult, []api.Message) {
	if !m.enabled || len(history) == 0 {
		return &MaskingResult{}, history
	}

	// ── Pass 1: backward scan to find protection boundary ──
	protected := 0
	cutoffIdx := 0
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		protected += m.msgTokens(msg)
		if protected >= m.protectionThreshold {
			cutoffIdx = i
			break
		}
	}

	// ── Pass 2: scan from 0 to cutoffIdx, count prunable ──
	prunable := 0
	for i := 0; i < cutoffIdx; i++ {
		if history[i].Role == "tool" && !m.isExempt(history[i].Name) && len(history[i].Content) > 100 {
			prunable += len(history[i].Content) / 4
		}
	}

	if prunable < m.minPrunableThreshold {
		return &MaskingResult{}, history
	}

	// ── Pass 3: mask prunable tool outputs ──
	if err := os.MkdirAll(m.outputDir, 0755); err != nil {
		log.Warnf("masker: cannot create output dir: %v", err)
		return &MaskingResult{}, history
	}

	tokensSaved := 0
	maskedCount := 0
	// Copy the history (shallow copy is fine; we replace content of specific messages)
	newHistory := make([]api.Message, len(history))
	copy(newHistory, history)

	for i := 0; i < cutoffIdx; i++ {
		if newHistory[i].Role != "tool" || m.isExempt(newHistory[i].Name) || len(newHistory[i].Content) <= 100 {
			continue
		}

		name := strings.ReplaceAll(newHistory[i].Name, "/", "_")
		filename := fmt.Sprintf("output_%d_%s.txt", i, name)
		filePath := filepath.Join(m.outputDir, filename)

		if err := os.WriteFile(filePath, []byte(newHistory[i].Content), 0644); err != nil {
			log.Warnf("masker: write failed: %v", err)
			continue
		}

		tokensSaved += len(newHistory[i].Content) / 4
		newHistory[i].Content = fmt.Sprintf("[toolu_vrtx_01Masked%s...] %d tokens masked to %s",
			strings.ReplaceAll(name, "_", " "), len(history[i].Content)/4, filePath)
		maskedCount++
	}

	return &MaskingResult{
		NewHistory:  newHistory,
		MaskedCount: maskedCount,
		TokensSaved: tokensSaved,
	}, newHistory
}

// msgTokens estimates the token count of a message.
func (m *ToolOutputMasker) msgTokens(msg api.Message) int {
	t := len(msg.Content)
	for _, tc := range msg.ToolCalls {
		t += len(tc.Name) + 50
	}
	return t / 4
}

// isExempt checks whether a tool name should never be masked.
func (m *ToolOutputMasker) isExempt(toolName string) bool {
	for _, part := range strings.Split(toolName, "__") {
		if m.exemptTools[part] {
			return true
		}
	}
	return m.exemptTools[toolName]
}
