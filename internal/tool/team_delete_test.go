package tool

import (
	"context"
	"testing"
)

// Member IDs are "team-<name>-<i>" and team_delete removed every task with
// the prefix "team-<name>-", so deleting team "a" also removed the members of
// team "a-b" (team-a-b-1).
func TestTeamDeleteLeavesTeamsWithTheNameAsPrefix(t *testing.T) {
	rt := &Runtime{Tasks: make(map[string]*TaskRecord)}
	for _, name := range []string{"a", "a-b", "a-1"} {
		res, _ := NewTeamCreateTool().Call(context.Background(), Input{"name": name, "members": []any{
			map[string]any{"agent": "x", "task": "t1"},
			map[string]any{"agent": "y", "task": "t2"},
		}}, Context{Runtime: rt})
		if res.IsError {
			t.Fatalf("team_create %s: %s", name, res.Data)
		}
	}
	// An unrelated task whose ID happens to look like a member of "a".
	rt.Tasks["team-a-3x"] = &TaskRecord{ID: "team-a-3x"}

	res, _ := NewTeamDeleteTool().Call(context.Background(), Input{"name": "a"}, Context{Runtime: rt})
	if res.IsError {
		t.Fatalf("team_delete: %s", res.Data)
	}
	for _, gone := range []string{"team-a-1", "team-a-2"} {
		if _, ok := rt.Tasks[gone]; ok {
			t.Errorf("%s was not deleted", gone)
		}
	}
	for _, kept := range []string{"team-a-b-1", "team-a-b-2", "team-a-1-1", "team-a-1-2", "team-a-3x"} {
		if _, ok := rt.Tasks[kept]; !ok {
			t.Errorf("deleting team a also deleted %s", kept)
		}
	}
	if _, ok := rt.Teams["a-b"]; !ok {
		t.Error("team a-b record was deleted")
	}
}

func TestIsTeamMemberID(t *testing.T) {
	for _, tc := range []struct {
		id, team string
		want     bool
	}{
		{"team-a-1", "a", true},
		{"team-a-12", "a", true},
		{"team-a-b-1", "a", false},
		{"team-a-1-2", "a", false},
		{"team-a-", "a", false},
		{"team-a-1x", "a", false},
		{"team-a-b-1", "a-b", true},
	} {
		if got := isTeamMemberID(tc.id, tc.team); got != tc.want {
			t.Errorf("isTeamMemberID(%q, %q) = %v, want %v", tc.id, tc.team, got, tc.want)
		}
	}
}
