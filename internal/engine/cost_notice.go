package engine

import "fmt"

// costBudgetNotice tells the user, once per budget, that the session's spend
// reached costBudgetNoticeRatio of max_budget_usd. The cap used to stop a
// turn without any warning. Changing the budget (SetMaxBudget) re-arms it.
func (e *Engine) costBudgetNotice() {
	if e.costTracker == nil {
		return
	}
	t := e.costTracker.Totals()
	if t.MaxBudget <= 0 || t.Cost < costBudgetNoticeRatio*t.MaxBudget {
		return
	}
	e.bgMu.Lock()
	if e.costNoticeFor == t.MaxBudget {
		e.bgMu.Unlock()
		return
	}
	e.costNoticeFor = t.MaxBudget
	e.bgMu.Unlock()
	e.engineOutput(fmt.Sprintf("  \x1b[33m费用已达预算的 %.0f%%：$%.2f / $%.2f（到达上限将停止；/budget <金额> 调整本会话上限，/budget off 取消上限）\x1b[0m",
		costBudgetNoticeRatio*100, t.Cost, t.MaxBudget))
}
