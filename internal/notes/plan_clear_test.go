package notes

import (
	"os"
	"strings"
	"testing"
)

// SetPlan("") marks the notes modified, but Flush returned early when no
// entries were kept and the plan was empty, so the cleared plan stayed on
// disk and the next session offered it as unfinished again.
func TestFlushPersistsClearedPlan(t *testing.T) {
	s, projectDir := newTestNotes(t)
	s.SetPlan("- [ ] write the migration\n- [ ] run it")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(s.path); !strings.Contains(string(data), planHeader) {
		t.Fatalf("plan not written:\n%s", data)
	}

	next := New(projectDir)
	next.Load()
	if plan, _ := next.Plan(); plan == "" {
		t.Fatal("fixture: the plan did not survive to the next session")
	}
	next.SetPlan("") // every item done
	if err := next.Flush(); err != nil {
		t.Fatal(err)
	}

	after := New(projectDir)
	after.Load()
	if plan, _ := after.Plan(); plan != "" {
		t.Fatalf("a cleared plan came back as unfinished: %q", plan)
	}
}

// Clearing the plan with nothing loaded keeps the rest of the file.
func TestClearedPlanKeepsOtherNotesOnDisk(t *testing.T) {
	s, projectDir := newTestNotes(t)
	s.AddDecision("use sqlite")
	s.SetPlan("- [ ] ship")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	fresh := New(projectDir) // not loaded: holds no entries
	fresh.plan = "- [ ] ship"
	fresh.SetPlan("")
	if err := fresh.Flush(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.path)
	if strings.Contains(string(data), planHeader) || !strings.Contains(string(data), "use sqlite") {
		t.Fatalf("notes file after clearing the plan:\n%s", data)
	}
}
