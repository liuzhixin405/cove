package api

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyByStatus verifies the classifiers read the HTTP status from
// *StatusError rather than guessing from the message text.
func TestClassifyByStatus(t *testing.T) {
	cases := []struct {
		status                        int
		rateLimit, temporary, permStr bool
	}{
		{429, true, false, false},
		{500, false, true, false},
		{503, false, true, false},
		{401, false, false, true},
		{403, false, false, true},
		{400, false, false, false},
		{404, false, false, false},
	}
	for _, tc := range cases {
		err := &StatusError{Status: tc.status, Msg: "boom"}
		if got := isRateLimit(err); got != tc.rateLimit {
			t.Errorf("status %d: isRateLimit = %v, want %v", tc.status, got, tc.rateLimit)
		}
		if got := isTemporary(err); got != tc.temporary {
			t.Errorf("status %d: isTemporary = %v, want %v", tc.status, got, tc.temporary)
		}
		if got := isPermanent(err); got != tc.permStr {
			t.Errorf("status %d: isPermanent = %v, want %v", tc.status, got, tc.permStr)
		}
	}
}

// TestClassifyIgnoresIncidentalDigits is the regression test for the bug that
// evicted healthy providers: a 400 whose body merely mentions a number
// containing "500" must not be treated as a server error.
func TestClassifyIgnoresIncidentalDigits(t *testing.T) {
	noisy := []error{
		&StatusError{Status: 400, Msg: "max_tokens must be under 1500 tokens"},
		&StatusError{Status: 400, Msg: "model gpt-4o-mini-2024-07-18 does not support tools"},
		&StatusError{Status: 422, Msg: "input exceeded 429000 characters"},
		&StatusError{Status: 400, Msg: "unknown parameter 'top_k' (code 4030)"},
	}
	for _, err := range noisy {
		if isRateLimit(err) {
			t.Errorf("%v wrongly classified as rate limit", err)
		}
		if isTemporary(err) {
			t.Errorf("%v wrongly classified as temporary", err)
		}
		if isPermanent(err) {
			t.Errorf("%v wrongly classified as permanent", err)
		}
	}
}

// TestClassifyWrappedStatus confirms errors.As reaches a wrapped *StatusError.
func TestClassifyWrappedStatus(t *testing.T) {
	wrapped := fmt.Errorf("provider call failed: %w", &StatusError{Status: 429, Msg: "slow down"})
	if !isRateLimit(wrapped) {
		t.Error("wrapped 429 not recognized as rate limit")
	}
	if statusOf(errors.New("plain")) != 0 {
		t.Error("statusOf on a plain error should be 0")
	}
}

// TestClassifyTransportFallback checks the text heuristics still cover
// transport-level errors, which carry no HTTP status at all.
func TestClassifyTransportFallback(t *testing.T) {
	for _, msg := range []string{
		"dial tcp: connection refused",
		"context deadline exceeded",
		"unexpected EOF",
		"lookup api.example.com: no such host",
	} {
		if !isTemporary(errors.New(msg)) {
			t.Errorf("%q should be temporary", msg)
		}
	}
	if !isRateLimit(errors.New("Rate limit reached for requests")) {
		t.Error("textual rate-limit message not recognized")
	}
	if !isPermanent(errors.New("Invalid API key provided")) {
		t.Error("textual auth failure not recognized")
	}
}
