package state

type AppState struct {
	SessionID      string
	Model          string
	PermissionMode string
	BudgetUsed     float64
	MaxBudget      float64
	Messages       int
	Debug          bool
}

func NewState() *AppState {
	return &AppState{}
}
