package diagnostic

import (
	"strings"
	"testing"
)

// A corrupt or oversized line in errors.log used to stop the scanner, and
// every event after it was silently lost.
func TestParseRuntimeLogSkipsBadLinesAndKeepsGoing(t *testing.T) {
	huge := strings.Repeat("x", 2<<20)
	input := `{"time":"2026-09-20T10:00:00+08:00","severity":2,"category":"api","message":"first"}` + "\n" +
		huge + "\n" +
		"not json\n" +
		`{"time":"2026-09-20T10:01:00+08:00","severity":2,"category":"api","message":"last"}` + "\n"
	events := parseRuntimeLog(strings.NewReader(input))
	if len(events) != 2 || events[0].Message != "first" || events[1].Message != "last" {
		t.Fatalf("events = %+v, want first and last", events)
	}
}
