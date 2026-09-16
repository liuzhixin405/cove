package plugin

import "testing"

// TestShortSHA locks in the H-6 fix: an empty or short commit SHA must not panic
// (the former lock.CommitSHA[:7] did on "").
func TestShortSHA(t *testing.T) {
	cases := map[string]string{
		"":             "", // was a slice-out-of-range panic
		"abc":          "abc",
		"abcdefg":      "abcdefg",
		"abcdefgh":     "abcdefg",
		"0123456789ab": "0123456",
	}
	for in, want := range cases {
		if got := shortSHA(in); got != want {
			t.Errorf("shortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}
