package command

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// /diagnose codes grouped the codes by category but listed each group in Go's
// random map order, so E1003 could come before E1001 and the list changed
// every time it was shown.
func TestDiagnoseCodesAreListedInOrder(t *testing.T) {
	out, err := NewDiagnoseCmd().Execute(context.Background(), Input{Args: []string{"codes"}})
	if err != nil {
		t.Fatal(err)
	}
	// Categories are printed in a fixed order under "[category]" headers;
	// within each section the codes must be sorted.
	header := regexp.MustCompile(`\[([a-z]+)\]`)
	code := regexp.MustCompile(`E\d{4}`)
	sections := map[string][]string{}
	current, total := "", 0
	for _, line := range strings.Split(out.Message, "\n") {
		if m := header.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if c := code.FindString(line); c != "" {
			sections[current] = append(sections[current], c)
			total++
		}
	}
	if total < 10 {
		t.Fatalf("only %d codes listed", total)
	}
	for cat, list := range sections {
		if !sort.StringsAreSorted(list) {
			t.Errorf("[%s] codes out of order: %v", cat, list)
		}
	}
}
