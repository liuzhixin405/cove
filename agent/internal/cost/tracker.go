package cost

import "strings"

type Price struct {
	Input  float64
	Output float64
}

var Prices = map[string]Price{
	"claude-sonnet-4-20250514":  {Input: 3.0, Output: 15.0},
	"claude-opus-4-20250514":    {Input: 15.0, Output: 75.0},
	"claude-haiku-3-5":          {Input: 0.8, Output: 4.0},
	"deepseek-chat":             {Input: 0.27, Output: 1.10},
	"deepseek-reasoner":         {Input: 0.55, Output: 2.19},
	"gpt-4o":                    {Input: 2.5, Output: 10.0},
	"gpt-4o-mini":               {Input: 0.15, Output: 0.6},
}

type Tracker struct {
	TotalInput  int
	TotalOutput int
	TotalCost   float64
	MaxBudget   float64
}

func NewTracker(maxBudget float64) *Tracker {
	return &Tracker{MaxBudget: maxBudget}
}

func (t *Tracker) Add(model string, input, output int) {
	t.TotalInput += input
	t.TotalOutput += output
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
	t.TotalCost += (float64(input)/1e6)*p.Input + (float64(output)/1e6)*p.Output
}

func (t *Tracker) OverBudget() bool {
	if t.MaxBudget <= 0 {
		return false
	}
	return t.TotalCost >= t.MaxBudget
}

func (t *Tracker) Summary() string {
	sb := &strings.Builder{}
	sb.WriteString(itoa(t.TotalInput) + " in | " + itoa(t.TotalOutput) + " out | $" + ftoa(t.TotalCost))
	if t.MaxBudget > 0 {
		sb.WriteString(" / $" + ftoa(t.MaxBudget))
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 { return "0" }
	d := []byte{}
	for n > 0 { d = append([]byte{byte('0'+n%10)}, d...); n /= 10 }
	return string(d)
}
func ftoa(f float64) string {
	s := strings.Builder{}
	whole := int(f)
	s.WriteString(itoa(whole))
	s.WriteByte('.')
	frac := int((f - float64(whole)) * 100)
	if frac < 10 { s.WriteByte('0') }
	s.WriteString(itoa(frac))
	return s.String()
}
