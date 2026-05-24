package cost

import (
	"strconv"
	"strings"
)

type Price struct {
	Input         float64
	InputCacheHit float64
	Output        float64
}

var Prices = map[string]Price{
	"claude-sonnet-4-20250514": {Input: 3.0, InputCacheHit: 3.0, Output: 15.0},
	"claude-opus-4-20250514":   {Input: 15.0, InputCacheHit: 15.0, Output: 75.0},
	"claude-haiku-3-5":         {Input: 0.8, InputCacheHit: 0.8, Output: 4.0},
	"deepseek-v4-flash":        {Input: 0.14, InputCacheHit: 0.0028, Output: 0.28},
	"deepseek-v4-pro":          {Input: 0.435, InputCacheHit: 0.003625, Output: 0.87},
	"deepseek-chat":            {Input: 0.14, InputCacheHit: 0.0028, Output: 0.28},
	"deepseek-reasoner":        {Input: 0.14, InputCacheHit: 0.0028, Output: 0.28},
	"gpt-4o":                   {Input: 2.5, InputCacheHit: 2.5, Output: 10.0},
	"gpt-4o-mini":              {Input: 0.15, InputCacheHit: 0.15, Output: 0.6},
}

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
		p = Prices["claude-sonnet-4-20250514"]
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

func (t *Tracker) Summary() string {
	sb := &strings.Builder{}
	sb.WriteString(itoa(t.TotalInput) + " in")
	if t.TotalPromptCacheHit > 0 || t.TotalPromptCacheMiss > 0 {
		sb.WriteString(" (cache hit " + itoa(t.TotalPromptCacheHit) + ", miss " + itoa(t.TotalPromptCacheMiss) + ")")
	}
	sb.WriteString(" | " + itoa(t.TotalOutput) + " out | $" + ftoa(t.TotalCost))
	if t.MaxBudget > 0 {
		sb.WriteString(" / $" + ftoa(t.MaxBudget))
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	d := []byte{}
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
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
