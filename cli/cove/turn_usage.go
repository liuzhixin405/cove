package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/cost"
	"github.com/liuzhixin405/cove-agent/internal/engine"
)

// turnUsage is what one turn has taken so far: the spinner shows it live
// ("12s · 上下文 38% · $0.041") and the turn ends with its summary. Neither
// tokens, cost, context use nor elapsed time was visible while a turn ran.
type turnUsage struct {
	eng   *engine.Engine
	start time.Time
	base  cost.Totals
}

func newTurnUsage(eng *engine.Engine, start time.Time) *turnUsage {
	u := &turnUsage{eng: eng, start: start}
	if t := eng.CostTracker(); t != nil {
		u.base = t.Totals()
	}
	return u
}

// delta is the tracker's totals since the turn started.
func (u *turnUsage) delta() cost.Totals {
	t := u.eng.CostTracker()
	if t == nil {
		return cost.Totals{}
	}
	now := t.Totals()
	return cost.Totals{Input: now.Input - u.base.Input, Output: now.Output - u.base.Output, Cost: now.Cost - u.base.Cost}
}

// contextPart is "上下文 38%", "" when the window is unknown.
func (u *turnUsage) contextPart(withTokens bool) string {
	tokens, window := u.eng.ContextUsage()
	if tokens <= 0 || window <= 0 {
		return ""
	}
	s := fmt.Sprintf("上下文 %d%%", tokens*100/window)
	if withTokens {
		s += fmt.Sprintf("（%s/%s）", humanTokens(tokens), humanTokens(window))
	}
	return s
}

// live is the spinner's status suffix.
func (u *turnUsage) live() string {
	parts := []string{humanElapsed(time.Since(u.start))}
	if c := u.contextPart(false); c != "" {
		parts = append(parts, c)
	}
	if d := u.delta(); d.Cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.3f", d.Cost))
	}
	return strings.Join(parts, " · ")
}

// summary is the line a turn ends with; "" when it made no model call.
func (u *turnUsage) summary() string {
	d := u.delta()
	if d.Input == 0 && d.Output == 0 {
		return ""
	}
	parts := []string{
		"本轮 " + humanElapsed(time.Since(u.start)),
		fmt.Sprintf("%s 输入 / %s 输出", humanTokens(d.Input), humanTokens(d.Output)),
	}
	if d.Cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", d.Cost))
	}
	if c := u.contextPart(true); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, " · ")
}

func humanElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
