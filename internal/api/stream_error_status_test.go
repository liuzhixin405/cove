package api

import (
	"net/http"
	"strings"
	"testing"
)

// An in-stream overloaded_error used to come back as a plain
// fmt.Errorf("provider stream error: ...") with no status: Classify said
// KindUnknown, isTemporary said false, so the engine's fallback never fired
// and the provider was never cooled down — while the same failure before the
// 200 header (HTTP 529) did both. The streamed form must classify exactly
// like the HTTP status it stands for.
func TestAnthropicStreamOverloadedErrorClassifiesLikeHTTP529(t *testing.T) {
	_, err := streamAnthropic(t, ""+
		"event: content_block_delta\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n"+
		"event: error\n"+
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	if err == nil {
		t.Fatal("expected the error event to be returned")
	}
	httpErr := &RetryableError{Status: 529, Msg: "Overloaded"}
	if got, want := Classify(err), Classify(httpErr); got != want {
		t.Fatalf("Classify(streamed overloaded_error) = %v, want %v (same as HTTP 529)", got, want)
	}
	if got, want := isTemporary(err), isTemporary(httpErr); got != want {
		t.Fatalf("isTemporary(streamed overloaded_error) = %v, want %v", got, want)
	}
	if st := statusOf(err); st != 529 {
		t.Fatalf("statusOf = %d, want 529", st)
	}
}

func TestAnthropicStreamErrorTypesMapToStatuses(t *testing.T) {
	cases := []struct {
		typ  string
		want int
		kind ErrorKind
	}{
		{"overloaded_error", 529, KindServerError},
		{"rate_limit_error", http.StatusTooManyRequests, KindRateLimit},
		{"api_error", http.StatusInternalServerError, KindServerError},
		{"authentication_error", http.StatusUnauthorized, KindAuth},
		{"permission_error", http.StatusForbidden, KindAuth},
		{"invalid_request_error", http.StatusBadRequest, KindBadRequest},
		{"not_found_error", http.StatusNotFound, KindUnknown},
	}
	for _, tc := range cases {
		_, err := streamAnthropic(t, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\""+tc.typ+"\",\"message\":\"m\"}}\n\n")
		if err == nil {
			t.Fatalf("%s: expected an error", tc.typ)
		}
		if st := statusOf(err); st != tc.want {
			t.Errorf("%s: statusOf = %d, want %d", tc.typ, st, tc.want)
		}
		if k := Classify(err); k != tc.kind {
			t.Errorf("%s: Classify = %v, want %v", tc.typ, k, tc.kind)
		}
	}
}

// An error object of unknown type still carries the provider's message, and
// carries no status rather than a made-up one.
func TestAnthropicStreamUnknownErrorTypeKeepsMessage(t *testing.T) {
	_, err := streamAnthropic(t, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"weird_error\",\"message\":\"something odd\"}}\n\n")
	if err == nil {
		t.Fatal("expected an error")
	}
	if statusOf(err) != 0 {
		t.Fatalf("unknown error type must not invent a status: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "weird_error") || !strings.Contains(got, "something odd") {
		t.Fatalf("error = %q, want the type and message", got)
	}
}

// OpenAI-compatible gateways report an upstream failure after the 200 header
// as {"error":{"code":503,...}}. The numeric code is the HTTP status.
func TestOpenAIStreamErrorNumericCodeClassifiesLikeHTTPStatus(t *testing.T) {
	cases := []struct {
		chunk string
		want  int
		kind  ErrorKind
	}{
		{`{"error":{"message":"upstream overloaded","code":503}}`, 503, KindServerError},
		{`{"error":{"message":"slow down","code":"429"}}`, 429, KindRateLimit},
		{`{"error":{"message":"bad key","type":"authentication_error"}}`, 401, KindAuth},
		{`{"error":{"message":"rate limited","type":"rate_limit_error"}}`, 429, KindRateLimit},
		{`{"error":{"message":"broken","type":"server_error"}}`, 500, KindServerError},
		{`{"error":{"message":"nope","code":"insufficient_quota"}}`, 0, KindUnknown},
	}
	for _, tc := range cases {
		_, _, err := streamOpenAI(t, "data: "+tc.chunk+"\n\ndata: [DONE]\n\n")
		if err == nil {
			t.Fatalf("%s: expected an error", tc.chunk)
		}
		if st := statusOf(err); st != tc.want {
			t.Errorf("%s: statusOf = %d, want %d", tc.chunk, st, tc.want)
		}
		if k := Classify(err); k != tc.kind {
			t.Errorf("%s: Classify = %v, want %v", tc.chunk, k, tc.kind)
		}
		if tc.want >= 500 && !isTemporary(err) {
			t.Errorf("%s: a streamed %d must be temporary so the fallback cools the provider", tc.chunk, tc.want)
		}
	}
}
