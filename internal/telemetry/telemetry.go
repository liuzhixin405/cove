package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event represents a single telemetry event.
type Event struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data,omitempty"`
}

// Recorder collects telemetry events with local aggregation.
// All data stays local by default; opt-in for remote reporting.
type Recorder struct {
	mu       sync.Mutex
	events   []Event
	filePath string
	enabled  bool
}

// NewRecorder creates a telemetry recorder with local storage.
func NewRecorder() *Recorder {
	home, _ := os.UserHomeDir()
	return &Recorder{
		filePath: filepath.Join(home, ".cove", "telemetry.json"),
		enabled:  false, // opt-in only
	}
}

// Enable turns on telemetry recording.
func (r *Recorder) Enable() { r.enabled = true }

// Disable turns off telemetry recording.
func (r *Recorder) Disable() { r.enabled = false }

// Record adds a telemetry event.
func (r *Recorder) Record(eventType string, data any) {
	if !r.enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Cap events at 1000 to prevent disk bloat
	if len(r.events) >= 1000 {
		r.events = r.events[500:]
	}

	r.events = append(r.events, Event{
		Type:      eventType,
		Timestamp: time.Now(),
		Data:      data,
	})
}

// RecordUsage captures common usage metrics.
func (r *Recorder) RecordUsage(model string, tokensIn, tokensOut int, cost float64, duration time.Duration) {
	r.Record("usage", map[string]any{
		"model":      model,
		"tokens_in":  tokensIn,
		"tokens_out": tokensOut,
		"cost":       cost,
		"duration_ms": duration.Milliseconds(),
	})
}

// RecordToolCall captures a tool usage event.
func (r *Recorder) RecordToolCall(toolName string, success bool, duration time.Duration) {
	r.Record("tool_call", map[string]any{
		"tool":     toolName,
		"success":   success,
		"duration_ms": duration.Milliseconds(),
	})
}

// Flush writes accumulated events to disk.
func (r *Recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.events) == 0 {
		return nil
	}

	// Append-only: read existing, merge, write back
	existing, _ := readEvents(r.filePath)
	all := append(existing, r.events...)
	// Cap at 5000
	if len(all) > 5000 {
		all = all[len(all)-5000:]
	}

	if err := os.MkdirAll(filepath.Dir(r.filePath), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(all)
	if err != nil {
		return err
	}

	r.events = nil
	return os.WriteFile(r.filePath, data, 0644)
}

// Stats returns current in-memory event counts by type.
func (r *Recorder) Stats() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string]int)
	for _, e := range r.events {
		counts[e.Type]++
	}
	return counts
}

func readEvents(path string) ([]Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var events []Event
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, err
	}
	return events, nil
}
