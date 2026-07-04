package engine

import (
	"sync"

	"github.com/liuzhixin405/cove/internal/cost"
)

// fastModelOutcomeWindow is a small ring buffer recording whether recent
// turns routed to the fast/mid-tier model ended up needing to give up
// (verify-gate retries exhausted, or the tool-failure circuit breaker
// tripped) rather than completing cleanly. It implements
// api.FailureRateSignal so ModelRouter's scoring strategy can route to the
// premium model more readily once the fast model is visibly struggling on
// this project — without needing a full evidence-ledger/analytics system.
type fastModelOutcomeWindow struct {
	mu       sync.Mutex
	outcomes []bool // true = failure/give-up
	max      int
}

func newFastModelOutcomeWindow(size int) *fastModelOutcomeWindow {
	if size <= 0 {
		size = 20
	}
	return &fastModelOutcomeWindow{max: size}
}

// Record appends one outcome, evicting the oldest entry once the window is
// full. Safe to call with a nil receiver (no-op), so callers don't need a
// separate nil check at every call site.
func (w *fastModelOutcomeWindow) Record(failed bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.outcomes = append(w.outcomes, failed)
	if len(w.outcomes) > w.max {
		w.outcomes = w.outcomes[len(w.outcomes)-w.max:]
	}
}

// RecentFastModelFailureRate implements api.FailureRateSignal.
func (w *fastModelOutcomeWindow) RecentFastModelFailureRate() float64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.outcomes) == 0 {
		return 0
	}
	fails := 0
	for _, o := range w.outcomes {
		if o {
			fails++
		}
	}
	return float64(fails) / float64(len(w.outcomes))
}

// costBudgetSignal adapts *cost.Tracker to api.BudgetSignal, so the router's
// scoring strategy can see remaining budget without internal/api needing to
// import internal/cost directly.
type costBudgetSignal struct{ tracker *cost.Tracker }

func (c costBudgetSignal) RemainingBudgetRatio() float64 {
	if c.tracker == nil || c.tracker.MaxBudget <= 0 {
		return 1
	}
	remaining := c.tracker.MaxBudget - c.tracker.TotalCost
	if remaining < 0 {
		remaining = 0
	}
	ratio := remaining / c.tracker.MaxBudget
	if ratio > 1 {
		ratio = 1
	}
	return ratio
}
