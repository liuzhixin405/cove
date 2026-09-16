package state

import (
	"reflect"
	"sync"
	"testing"
)

// TestNewStateReturnsZeroValuedState pins the contract that NewState hands back
// a blank slate: cli/cove/app_bootstrap.go immediately assigns Model,
// ModelFast, PermissionMode, MaxBudget and Debug from the config, and
// internal/command renders whatever it finds. A silent default injected here
// (say MaxBudget = 10) would surface as a budget the user never configured.
func TestNewStateReturnsZeroValuedState(t *testing.T) {
	got := NewState()
	if got == nil {
		t.Fatal("NewState() returned nil")
	}
	if *got != (AppState{}) {
		t.Fatalf("NewState() = %+v, want the zero AppState", *got)
	}

	// Field-by-field, so the failure message names the offending field even if
	// a future field type makes the struct non-comparable.
	v := reflect.ValueOf(*got)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.IsZero() {
			t.Errorf("NewState().%s = %v, want the zero value", v.Type().Field(i).Name, f.Interface())
		}
	}
}

func TestNewStateReturnsDistinctInstances(t *testing.T) {
	a := NewState()
	b := NewState()

	if a == b {
		t.Fatal("NewState() returned the same pointer twice; callers would share one session's state")
	}

	a.SessionID = "session-a"
	a.Messages = 3
	a.BudgetUsed = 1.5

	if b.SessionID != "" || b.Messages != 0 || b.BudgetUsed != 0 {
		t.Fatalf("mutating one state leaked into another: %+v", *b)
	}
}

// TestAppStateFieldSchema pins the exact field set and types that other
// packages depend on:
//
//   - internal/command/commands_session.go formats BudgetUsed/MaxBudget with
//     %.2f (so they must stay floating point) and Messages with %d.
//   - internal/command/commands_session_misc.go assigns SessionID, Model,
//     Messages and BudgetUsed from a session record.
//   - cli/cove/app_bootstrap.go assigns Model, ModelFast, PermissionMode,
//     MaxBudget and Debug.
//
// Renaming or retyping any of these breaks those call sites, and adding a
// field without wiring it up is worth a deliberate decision, so the comparison
// is exact in both directions.
func TestAppStateFieldSchema(t *testing.T) {
	want := map[string]reflect.Kind{
		"SessionID":      reflect.String,
		"Model":          reflect.String,
		"ModelFast":      reflect.String,
		"PermissionMode": reflect.String,
		"BudgetUsed":     reflect.Float64,
		"MaxBudget":      reflect.Float64,
		"Messages":       reflect.Int,
		"Debug":          reflect.Bool,
	}

	typ := reflect.TypeOf(AppState{})
	got := make(map[string]reflect.Kind, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			t.Errorf("unexported field %s: AppState is written directly by other packages, so every field must be exported", f.Name)
			continue
		}
		got[f.Name] = f.Type.Kind()
	}

	for name, kind := range want {
		actual, ok := got[name]
		if !ok {
			t.Errorf("AppState is missing field %s (%s), which other packages assign", name, kind)
			continue
		}
		if actual != kind {
			t.Errorf("AppState.%s is %s, want %s", name, actual, kind)
		}
	}
	for name, kind := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("AppState gained field %s (%s): wire it into the consumers in internal/command and cli/cove, then add it here", name, kind)
		}
	}
}

// TestAppStateHasNoAliasingFields guards value-copy safety: AppState is small
// and comparable today, so `copy := *st` is a genuine snapshot. A map, slice,
// pointer or channel field would make such a copy share memory with the
// original and would also make the struct non-comparable.
func TestAppStateHasNoAliasingFields(t *testing.T) {
	typ := reflect.TypeOf(AppState{})
	if !typ.Comparable() {
		t.Fatalf("AppState is no longer comparable; == and value snapshots of it are broken")
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		switch f.Type.Kind() {
		case reflect.Map, reflect.Slice, reflect.Ptr, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Interface:
			t.Errorf("field %s is a %s: a value copy of AppState would alias it", f.Name, f.Type.Kind())
		}
	}
}

// TestAppStateDistinctFieldsAreIndependentUnderConcurrency documents what the
// package does and does not promise. AppState carries no mutex, so concurrent
// writers of the SAME field must be synchronized by the caller. What the layout
// does guarantee is that distinct fields are distinct memory: a writer of
// Messages cannot corrupt BudgetUsed. This is checked under -race because a
// future change that packs fields behind shared storage (bit flags, a shared
// `any`, a single map) would turn these independent writes into a data race.
func TestAppStateDistinctFieldsAreIndependentUnderConcurrency(t *testing.T) {
	st := NewState()

	const n = 500
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(4)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < n; i++ {
			st.Messages = i + 1
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < n; i++ {
			st.BudgetUsed = float64(i+1) / 4
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < n; i++ {
			st.Debug = i%2 == 0
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < n; i++ {
			st.SessionID = "session"
		}
	}()

	close(start)
	wg.Wait()

	if st.Messages != n {
		t.Errorf("Messages = %d, want %d", st.Messages, n)
	}
	if want := float64(n) / 4; st.BudgetUsed != want {
		t.Errorf("BudgetUsed = %v, want %v", st.BudgetUsed, want)
	}
	if st.SessionID != "session" {
		t.Errorf("SessionID = %q, want %q", st.SessionID, "session")
	}
	// Fields nobody wrote must still be untouched.
	if st.Model != "" || st.ModelFast != "" || st.PermissionMode != "" || st.MaxBudget != 0 {
		t.Errorf("unwritten fields were modified: %+v", *st)
	}
}
