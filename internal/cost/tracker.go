package cost

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Price struct {
	Input         float64
	InputCacheHit float64
	Output        float64
}

var Prices = map[string]Price{
	"claude-3-7-sonnet": {Input: 3.0, InputCacheHit: 0.30, Output: 15.0},
	"claude-3-5-sonnet": {Input: 3.0, InputCacheHit: 0.30, Output: 15.0},
	"claude-3-5-haiku":  {Input: 0.8, InputCacheHit: 0.08, Output: 4.0},
	"claude-3-opus":     {Input: 15.0, InputCacheHit: 1.5, Output: 75.0},
	"deepseek-chat":     {Input: 0.14, InputCacheHit: 0.14 * 0.1, Output: 0.28},
	"deepseek-reasoner": {Input: 0.14, InputCacheHit: 0.14 * 0.1, Output: 0.28},
	"deepseek-v4-flash": {Input: 0.14, InputCacheHit: 0.14 * 0.1, Output: 0.28},
	"deepseek-v4-pro":   {Input: 0.14, InputCacheHit: 0.14 * 0.1, Output: 0.28},
	"gpt-4o":            {Input: 2.5, InputCacheHit: 1.25, Output: 10.0},
	"gpt-4o-mini":       {Input: 0.15, InputCacheHit: 0.075, Output: 0.6},
	"o3-mini":           {Input: 1.1, InputCacheHit: 1.1, Output: 4.4},
}

// defaultPrice is used when a model name does not match any entry in Prices.
// Provides a conservative non-zero fallback so unknown models still produce a
// cost estimate (per-million-token USD rates).
var defaultPrice = Price{Input: 0.435, InputCacheHit: 0.003625, Output: 0.87}

type Tracker struct {
	TotalInput           int
	TotalOutput          int
	TotalPromptCacheHit  int
	TotalPromptCacheMiss int
	TotalCost            float64
	MaxBudget            float64
}

func NewTracker(maxBudget float64) *Tracker {
	return &Tracker{MaxBudget: maxBudget}
}

func (t *Tracker) Add(model string, input, output int) {
	t.AddDetailed(model, input, output, 0, 0)
}

func (t *Tracker) AddDetailed(model string, input, output, cacheHit, cacheMiss int) {
	t.TotalInput += input
	t.TotalOutput += output
	if cacheHit < 0 {
		cacheHit = 0
	}
	if cacheMiss < 0 {
		cacheMiss = 0
	}
	if cacheHit > input {
		cacheHit = input
	}
	if cacheHit+cacheMiss > input {
		cacheMiss = input - cacheHit
	}
	if cacheHit+cacheMiss < input {
		cacheMiss += input - (cacheHit + cacheMiss)
	}
	t.TotalPromptCacheHit += cacheHit
	t.TotalPromptCacheMiss += cacheMiss
	p, ok := Prices[model]
	if !ok {
		p = defaultPrice
		for k, v := range Prices {
			if strings.Contains(model, k) {
				p = v
				break
			}
		}
	}
	if p.InputCacheHit == 0 {
		p.InputCacheHit = p.Input
	}
	t.TotalCost += (float64(cacheMiss)/1e6)*p.Input + (float64(cacheHit)/1e6)*p.InputCacheHit + (float64(output)/1e6)*p.Output
}

func (t *Tracker) OverBudget() bool {
	if t.MaxBudget <= 0 {
		return false
	}
	return t.TotalCost >= t.MaxBudget
}

// SuggestedBudget returns an auto-increased budget target based on current spend.
// The result is rounded up to 2 decimals and is always higher than TotalCost.
func (t *Tracker) SuggestedBudget() float64 {
	base := t.TotalCost
	if base < 0 {
		base = 0
	}
	// Keep a practical headroom so one extra turn can complete.
	target := base * 1.2
	if target-base < 2 {
		target = base + 2
	}
	return math.Ceil(target*100) / 100
}

func (t *Tracker) Summary() string {
	sb := &strings.Builder{}
	sb.WriteString(strconv.Itoa(t.TotalInput) + " in")
	if t.TotalPromptCacheHit > 0 || t.TotalPromptCacheMiss > 0 {
		sb.WriteString(" (cache hit " + strconv.Itoa(t.TotalPromptCacheHit) + ", miss " + strconv.Itoa(t.TotalPromptCacheMiss) + ")")
	}
	sb.WriteString(" | " + strconv.Itoa(t.TotalOutput) + " out | $" + ftoa(t.TotalCost))
	if t.MaxBudget > 0 {
		sb.WriteString(" / $" + ftoa(t.MaxBudget))
	}
	return sb.String()
}

func ftoa(f float64) string {
	if f == 0 {
		return "0.00"
	}
	if f < 0.0001 {
		return "0.00"
	}
	if f < 0.01 {
		return strconv.FormatFloat(f, 'f', 4, 64)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// --- Cost Persistence ---

// CostRecord represents a persisted cost entry for a session.
type CostRecord struct {
	SessionID string    `json:"session_id"`
	Model     string    `json:"model"`
	Input     int       `json:"input"`
	Output    int       `json:"output"`
	CacheHit  int       `json:"cache_hit"`
	CacheMiss int       `json:"cache_miss"`
	Cost      float64   `json:"cost"`
	Timestamp time.Time `json:"timestamp"`
}

// CostHistory manages persistent cost records across sessions.
type CostHistory struct {
	path      string
	LoadError error        `json:"-"`
	Records   []CostRecord `json:"records"`
}

// NewCostHistory loads or creates a cost history file.
func NewCostHistory() *CostHistory {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cove")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "cost_history.json")

	h := &CostHistory{path: path}
	h.load()
	return h
}

func (h *CostHistory) load() {
	data, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &h.Records); err != nil {
		h.LoadError = err
		h.Records = nil
	}
}

// Save persists the current cost history to disk.
func (h *CostHistory) Save() error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(h.path, data, 0600)
}

// Add records a new cost entry.
func (h *CostHistory) Add(sessionID, model string, t *Tracker) {
	h.Records = append(h.Records, CostRecord{
		SessionID: sessionID,
		Model:     model,
		Input:     t.TotalInput,
		Output:    t.TotalOutput,
		CacheHit:  t.TotalPromptCacheHit,
		CacheMiss: t.TotalPromptCacheMiss,
		Cost:      t.TotalCost,
		Timestamp: time.Now(),
	})
	// Keep at most last 100 records
	if len(h.Records) > 100 {
		h.Records = h.Records[len(h.Records)-100:]
	}
}

// TotalAllTime returns the sum of all historical costs.
func (h *CostHistory) TotalAllTime() float64 {
	var total float64
	for _, r := range h.Records {
		total += r.Cost
	}
	return total
}

// Last7Days returns the sum of costs in the last 7 days.
func (h *CostHistory) Last7Days() float64 {
	var total float64
	cutoff := time.Now().AddDate(0, 0, -7)
	for _, r := range h.Records {
		if r.Timestamp.After(cutoff) {
			total += r.Cost
		}
	}
	return total
}

// Last24Hours returns the sum of costs in the last 24 hours.
func (h *CostHistory) Last24Hours() float64 {
	var total float64
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, r := range h.Records {
		if r.Timestamp.After(cutoff) {
			total += r.Cost
		}
	}
	return total
}
