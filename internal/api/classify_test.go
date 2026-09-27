package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyRecognisesEveryKind(t *testing.T) {
	llama := &StatusError{Status: 400, Msg: `{"error":{"code":400,"message":"request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":17964,"n_ctx":16384}}`}
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"nil", nil, KindUnknown},
		{"canceled", context.Canceled, KindCanceled},
		{"wrapped canceled", fmt.Errorf("api: %w", context.Canceled), KindCanceled},
		{"deadline", context.DeadlineExceeded, KindTimeout},
		{"timeout text", errors.New("Post http://127.0.0.1:1234: timeout awaiting headers"), KindTimeout},
		{"llama.cpp context", llama, KindContextLength},
		{"wrapped context", fmt.Errorf("api: %w", llama), KindContextLength},
		{"openai context", &StatusError{Status: 400, Msg: "This model's maximum context length is 65536 tokens"}, KindContextLength},
		{"413", &StatusError{Status: 413, Msg: "request_too_large"}, KindContextLength},
		{"429", &StatusError{Status: 429, Msg: "slow down"}, KindRateLimit},
		{"retryable 429", &RetryableError{Status: 429, Msg: "x"}, KindRateLimit},
		{"401", &StatusError{Status: 401, Msg: "bad key"}, KindAuth},
		{"403", &StatusError{Status: 403, Msg: "forbidden"}, KindAuth},
		{"400 other", &StatusError{Status: 400, Msg: "max_tokens must be at most 8192"}, KindBadRequest},
		{"503", &StatusError{Status: 503, Msg: "busy"}, KindServerError},
		{"retryable transport", &RetryableError{Status: 0, Msg: "connection reset by peer"}, KindTransport},
		{"refused", errors.New("dial tcp 127.0.0.1:1234: connectex: connection refused"), KindUnreachable},
		{"dns", errors.New("dial tcp: lookup api.example: no such host"), KindUnreachable},
		{"plain transport", errors.New("read tcp: connection reset by peer"), KindTransport},
		{"eof", errors.New("unexpected EOF"), KindTransport},
		{"tool args", &ToolArgsInvalidError{Tool: "bash"}, KindToolArgs},
		{"provider unavailable wraps context", &ProviderUnavailableError{Provider: "openai-compatible", Fails: 3, Cause: llama}, KindProviderUnavailable},
		{"unknown", errors.New("boom"), KindUnknown},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%s: Classify = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestErrorKindString(t *testing.T) {
	if KindContextLength.String() != "context_length" || KindUnknown.String() != "unknown" {
		t.Errorf("String() = %q / %q", KindContextLength, KindUnknown)
	}
}

func TestProviderUnavailableErrorUnwraps(t *testing.T) {
	cause := &StatusError{Status: 400, Msg: "context size"}
	err := &ProviderUnavailableError{Provider: "p", Fails: 3, Cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("Unwrap does not reach the cause")
	}
	if !IsContextLengthError(err) {
		t.Fatal("the cause's context-length nature is hidden by the wrapper")
	}
}
