package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rawSSEServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func meterStream(t *testing.T, p Provider) (billed []*ChatResponse, err error) {
	t.Helper()
	m := NewMeteredProvider(p, func(_ string, resp *ChatResponse) { billed = append(billed, resp) })
	_, err = m.ChatStream(context.Background(), hiRequest, nil)
	return billed, err
}

// A stream that fails after the model generated is billed by the provider,
// but the meter used to see only the error, so max_budget_usd undercounted
// every stall, cut-off stream and in-stream error.
func TestMeterBillsAStreamThatFailsAfterGenerating(t *testing.T) {
	long := strings.Repeat("generated text ", 40)
	cases := []struct {
		name    string
		p       func(url string, c *http.Client) Provider
		body    string
		wantIn  int
		wantOut int // 0: estimated, must be > 0
	}{
		{
			name: "openai cut off without usage",
			p: func(u string, c *http.Client) Provider {
				return &openAICompatProvider{apiKey: "k", baseURL: u, client: c}
			},
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + long + "\"}}]}\n\n",
		},
		{
			name: "openai error after usage",
			p: func(u string, c *http.Client) Provider {
				return &openAICompatProvider{apiKey: "k", baseURL: u, client: c}
			},
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":30}}\n\n" +
				"data: {\"error\":{\"message\":\"upstream overloaded\"}}\n\n",
			wantIn: 120, wantOut: 30,
		},
		{
			name: "anthropic error event",
			p: func(u string, c *http.Client) Provider {
				return &anthropicProvider{apiKey: "k", baseURL: u, client: c}
			},
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":1}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + long + "\"}}\n\n" +
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
			wantIn: 200,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := rawSSEServer(t, tc.body)
			billed, err := meterStream(t, tc.p(srv.URL, srv.Client()))
			if err == nil {
				t.Fatal("the stream should fail")
			}
			if len(billed) != 1 {
				t.Fatalf("billed %d times, want once (err: %v)", len(billed), err)
			}
			got := billed[0]
			if got.InputTokens != tc.wantIn {
				t.Errorf("input tokens = %d, want %d", got.InputTokens, tc.wantIn)
			}
			if tc.wantOut != 0 && got.OutputTokens != tc.wantOut {
				t.Errorf("output tokens = %d, want %d", got.OutputTokens, tc.wantOut)
			}
			if tc.wantOut == 0 && got.OutputTokens < 20 {
				t.Errorf("output tokens = %d, want an estimate of the streamed text", got.OutputTokens)
			}
		})
	}
}

// A request that failed before anything streamed has nothing to bill.
func TestMeterDoesNotBillAStreamRejectedUpFront(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()
	billed, err := meterStream(t, &openAICompatProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()})
	if err == nil || len(billed) != 0 {
		t.Fatalf("err=%v billed=%d, want an error and no bill", err, len(billed))
	}
}
