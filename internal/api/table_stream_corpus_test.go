package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Corpus for TestTableStream (table_stream_test.go). Every case is run under
// every chunking in t5Chunkings; expectations are per logical reply.

func t5JS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- Anthropic Messages streaming events ---

func aEv(typ, data string) string { return "event: " + typ + "\ndata: " + data + "\n\n" }

func aMsgStart(usage string) string {
	return aEv("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":`+usage+`}}`)
}

func aBlockStart(idx int, block string) string {
	return aEv("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, idx, block))
}

func aTextStart(idx int) string { return aBlockStart(idx, `{"type":"text","text":""}`) }

func aDelta(idx int, delta string) string {
	return aEv("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, idx, delta))
}

func aText(idx int, s string) string { return aDelta(idx, `{"type":"text_delta","text":`+t5JS(s)+`}`) }

func aJSON(idx int, partial string) string {
	return aDelta(idx, `{"type":"input_json_delta","partial_json":`+t5JS(partial)+`}`)
}

func aToolStart(idx int, id, name string) string {
	return aBlockStart(idx, `{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":{}}`)
}

func aBlockStop(idx int) string {
	return aEv("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
}

func aMsgDelta(stop, usage string) string {
	d := `{"type":"message_delta","delta":{"stop_reason":"` + stop + `","stop_sequence":null}`
	if usage != "" {
		d += `,"usage":` + usage
	}
	return aEv("message_delta", d+"}")
}

func aMsgStop() string { return aEv("message_stop", `{"type":"message_stop"}`) }

func aError(typ string) string {
	return aEv("error", `{"type":"error","error":{"type":"`+typ+`","message":"boom"}}`)
}

func aHTTPBody(typ string) string {
	return `{"type":"error","error":{"type":"` + typ + `","message":"boom"}}`
}

// --- OpenAI chat.completions streaming chunks ---

func oEv(data string) string { return "data: " + data + "\n\n" }

func oChunk(delta, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return oEv(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fr + `}],"usage":null}`)
}

func oRole() string { return oChunk(`{"role":"assistant","content":""}`, "") }

func oText(s string) string { return oChunk(`{"content":`+t5JS(s)+`}`, "") }

func oReason(s string) string { return oChunk(`{"reasoning_content":`+t5JS(s)+`}`, "") }

func oFinish(fr string) string { return oChunk(`{}`, fr) }

func oTC(tcs string) string { return oChunk(`{"tool_calls":`+tcs+`}`, "") }

func oUsage(u string) string {
	return oEv(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[],"usage":` + u + `}`)
}

func oDone() string { return "data: [DONE]\n\n" }

func oErr(obj string) string { return oEv(`{"error":` + obj + `}`) }

// t5Big is a ~200KB string without JSON escapes.
var t5Big = strings.Repeat("abcdefghij", 20000)

func t5Corpus() []t5Case {
	const aUsageStart = `{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}`
	const aUsageEnd = `{"output_tokens":7}`
	// aTextBody is the plain text reply shared by several Anthropic cases.
	aTextBody := []string{
		aMsgStart(aUsageStart),
		aTextStart(0),
		aText(0, "Hel"), aText(0, "lo "), aText(0, "世界"),
		aBlockStop(0),
		aMsgDelta("end_turn", aUsageEnd),
		aMsgStop(),
	}
	aTextWant := &t5View{Content: "Hello 世界", Stop: "end_turn", In: 15, Out: 7, Hit: 3, Miss: 12, Write: 2}
	aTextNonStream := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"Hello 世界"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`

	const oUsageFinal = `{"prompt_tokens":10,"completion_tokens":7,"total_tokens":17,"prompt_tokens_details":{"cached_tokens":4}}`
	oTextBody := []string{
		oRole(), oText("Hel"), oText("lo "), oText("世界"), oFinish("stop"), oUsage(oUsageFinal), oDone(),
	}
	oTextWant := &t5View{Content: "Hello 世界", Stop: "stop", In: 10, Out: 7, Hit: 4, Miss: 6}
	oTextNonStream := `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello 世界"},"finish_reason":"stop"}],"usage":` + oUsageFinal + `}`

	cases := []t5Case{
		// ---------------- Anthropic ----------------
		{
			name: "text-deltas", provider: t5Anthropic, note: "text deltas incl. multi-byte runes split by bytewise chunking; usage split start/end",
			events: aTextBody, want: aTextWant, nonStream: aTextNonStream,
		},
		{
			name: "text-with-sse-lookalikes", provider: t5Anthropic, note: "text containing escaped newlines and \"data: \" must not be taken as SSE framing",
			events: []string{
				aMsgStart(`{"input_tokens":4,"output_tokens":1}`),
				aTextStart(0),
				aText(0, "line1\ndata: [DONE]\n\n"), aText(0, ": not a comment\r\nend"),
				aBlockStop(0), aMsgDelta("end_turn", `{"output_tokens":9}`), aMsgStop(),
			},
			want:      &t5View{Content: "line1\ndata: [DONE]\n\n: not a comment\r\nend", Stop: "end_turn", In: 4, Out: 9, Miss: 4},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"text","text":` + t5JS("line1\ndata: [DONE]\n\n: not a comment\r\nend") + `}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":9}}`,
		},
		{
			name: "noise-comments-ping-unknown-empty", provider: t5Anthropic, note: "comment lines, ping, unknown event type, empty data line, id:/retry: fields and a trailing [DONE] are ignored",
			events: []string{
				": keepalive\n\n",
				aMsgStart(aUsageStart),
				aEv("ping", `{"type":"ping"}`),
				aTextStart(0),
				"id: 7\nretry: 100\n\n",
				aText(0, "Hel"),
				"data:\n\n",
				aEv("some_future_event", `{"type":"some_future_event","index":0,"foo":{"bar":1}}`),
				aText(0, "lo "),
				": keepalive\n\n",
				aText(0, "世界"),
				aBlockStop(0), aMsgDelta("end_turn", aUsageEnd), aMsgStop(),
				oDone(),
			},
			want: aTextWant, nonStream: aTextNonStream,
		},
		{
			name: "data-without-space", provider: t5Anthropic, note: "\"data:\" without the optional space",
			events: []string{
				"event: message_start\ndata:" + `{"type":"message_start","message":{"usage":{"input_tokens":2,"output_tokens":1}}}` + "\n\n",
				"data:" + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n",
				"data:" + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n",
				"data:" + `{"type":"message_stop"}` + "\n\n",
			},
			want: &t5View{Content: "ok", Stop: "end_turn", In: 2, Out: 1, Miss: 2},
		},
		{
			name: "tool-args-split", provider: t5Anthropic, note: "input_json_delta split mid-key and mid-string",
			events: []string{
				aMsgStart(`{"input_tokens":20,"output_tokens":1}`),
				aTextStart(0), aText(0, "Let me look."), aBlockStop(0),
				aToolStart(1, "toolu_1", "read_file"),
				aJSON(1, ""), aJSON(1, `{"pa`), aJSON(1, `th": "a.go`), aJSON(1, `", "limit": 5}`),
				aBlockStop(1),
				aMsgDelta("tool_use", `{"output_tokens":30}`), aMsgStop(),
			},
			want: &t5View{Content: "Let me look.", Stop: "tool_use", In: 20, Out: 30, Miss: 20,
				ToolCalls: []t5TC{{ID: "toolu_1", Name: "read_file", Input: `{"limit":5,"path":"a.go"}`}}},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"text","text":"Let me look."},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.go","limit":5}}],"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":30}}`,
		},
		{
			name: "two-tools-interleaved", provider: t5Anthropic, note: "deltas of two tool_use blocks interleaved: merged by content index, ordered by index",
			events: []string{
				aMsgStart(`{"input_tokens":5,"output_tokens":1}`),
				aToolStart(0, "toolu_a", "grep"),
				aToolStart(1, "toolu_b", "glob"),
				aJSON(1, `{"pattern":`), aJSON(0, `{"q":"fo`), aJSON(1, `"*.go"}`), aJSON(0, `o"}`),
				aBlockStop(0), aBlockStop(1),
				aMsgDelta("tool_use", `{"output_tokens":12}`), aMsgStop(),
			},
			want: &t5View{Stop: "tool_use", In: 5, Out: 12, Miss: 5, ToolCalls: []t5TC{
				{ID: "toolu_a", Name: "grep", Input: `{"q":"foo"}`},
				{ID: "toolu_b", Name: "glob", Input: `{"pattern":"*.go"}`},
			}},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_a","name":"grep","input":{"q":"foo"}},{"type":"tool_use","id":"toolu_b","name":"glob","input":{"pattern":"*.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":12}}`,
		},
		{
			name: "tool-no-args", provider: t5Anthropic, note: "a tool without parameters streams no input_json_delta: empty input, not dropped",
			events: []string{
				aMsgStart(`{"input_tokens":5,"output_tokens":1}`),
				aToolStart(0, "toolu_n", "list_tasks"), aBlockStop(0),
				aMsgDelta("tool_use", `{"output_tokens":3}`), aMsgStop(),
			},
			want:      &t5View{Stop: "tool_use", In: 5, Out: 3, Miss: 5, ToolCalls: []t5TC{{ID: "toolu_n", Name: "list_tasks", Input: `{}`}}},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_n","name":"list_tasks","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`,
		},
		{
			name: "thinking-then-text", provider: t5Anthropic, note: "thinking_delta/signature_delta accumulate into one thinking block; reasoning callbacks",
			events: []string{
				aMsgStart(`{"input_tokens":8,"output_tokens":1}`),
				aBlockStart(0, `{"type":"thinking","thinking":"","signature":""}`),
				aDelta(0, `{"type":"thinking_delta","thinking":"Let me "}`),
				aDelta(0, `{"type":"thinking_delta","thinking":"think."}`),
				aDelta(0, `{"type":"signature_delta","signature":"sig123"}`),
				aBlockStop(0),
				aTextStart(1), aText(1, "Answer"), aBlockStop(1),
				aMsgDelta("end_turn", `{"output_tokens":11}`), aMsgStop(),
			},
			want: &t5View{Content: "Answer", Stop: "end_turn", In: 8, Out: 11, Miss: 8,
				Thinking: []string{`{"signature":"sig123","thinking":"Let me think.","type":"thinking"}`}},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"thinking","thinking":"Let me think.","signature":"sig123"},{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":11}}`,
		},
		{
			name: "redacted-thinking", provider: t5Anthropic, note: "redacted_thinking arrives whole at content_block_start and is kept verbatim",
			events: []string{
				aMsgStart(`{"input_tokens":8,"output_tokens":1}`),
				aBlockStart(0, `{"type":"redacted_thinking","data":"EnCr=="}`), aBlockStop(0),
				aTextStart(1), aText(1, "Hi"), aBlockStop(1),
				aMsgDelta("end_turn", `{"output_tokens":2}`), aMsgStop(),
			},
			want: &t5View{Content: "Hi", Stop: "end_turn", In: 8, Out: 2, Miss: 8,
				Thinking: []string{`{"data":"EnCr==","type":"redacted_thinking"}`}},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"redacted_thinking","data":"EnCr=="},{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":2}}`,
		},
		{
			name: "usage-end-only", provider: t5Anthropic, note: "no message_start usage; message_delta carries input and output",
			events: []string{
				aTextStart(0), aText(0, "x"), aBlockStop(0),
				aMsgDelta("end_turn", `{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":1}`), aMsgStop(),
			},
			want:      &t5View{Content: "x", Stop: "end_turn", In: 6, Out: 3, Hit: 1, Miss: 5},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":1}}`,
		},
		{
			name: "max-tokens", provider: t5Anthropic, note: "stop_reason max_tokens is reported as such",
			events: []string{
				aMsgStart(`{"input_tokens":3,"output_tokens":1}`),
				aTextStart(0), aText(0, "trunc"), aBlockStop(0),
				aMsgDelta("max_tokens", `{"output_tokens":64}`), aMsgStop(),
			},
			want:      &t5View{Content: "trunc", Stop: "max_tokens", In: 3, Out: 64, Miss: 3},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"text","text":"trunc"}],"stop_reason":"max_tokens","usage":{"input_tokens":3,"output_tokens":64}}`,
		},
		{
			name: "close-after-stop-reason", provider: t5Anthropic, note: "message_delta with stop_reason seen, message_stop lost: accepted (code comment: only no stop_reason AND no message_stop is incomplete)",
			events: []string{
				aMsgStart(`{"input_tokens":3,"output_tokens":1}`),
				aTextStart(0), aText(0, "done"), aBlockStop(0),
				aMsgDelta("end_turn", `{"output_tokens":2}`),
			},
			want: &t5View{Content: "done", Stop: "end_turn", In: 3, Out: 2, Miss: 3},
		},
		{
			name: "close-early", provider: t5Anthropic, note: "server closes cleanly after a text delta: incomplete reply is an error, not end_turn",
			events: []string{
				aMsgStart(`{"input_tokens":3,"output_tokens":1}`),
				aTextStart(0), aText(0, "half"),
			},
			wantErr: &t5Err{Kind: KindTransport, Temp: true},
		},
		{
			name: "close-before-any-event", provider: t5Anthropic, note: "200 header then empty body",
			events:  []string{},
			wantErr: &t5Err{Kind: KindTransport, Temp: true},
		},
		{
			name: "abort-mid-stream", provider: t5Anthropic, term: t5Abort, note: "connection dropped mid-stream (no chunked terminator)",
			events:  []string{aMsgStart(`{"input_tokens":3,"output_tokens":1}`), aTextStart(0), aText(0, "half")},
			wantErr: &t5Err{Kind: KindTransport, Temp: true},
		},
		{
			name: "hang-watchdog", provider: t5Anthropic, term: t5Hang, note: "server stops sending: idle watchdog ends the stream as a stall (timeout, temporary)",
			events:  []string{aMsgStart(`{"input_tokens":3,"output_tokens":1}`), aTextStart(0), aText(0, "half")},
			wantErr: &t5Err{Kind: KindTimeout, Temp: true},
		},
		{
			name: "ctx-cancel", provider: t5Anthropic, term: t5Cancel, note: "caller cancels mid-stream: canceled, not a stall or transport error",
			events:  []string{aMsgStart(`{"input_tokens":3,"output_tokens":1}`), aTextStart(0), aText(0, "half")},
			wantErr: &t5Err{Kind: KindCanceled},
		},

		{
			name: "complete-then-hang", provider: t5Anthropic, term: t5Hang,
			note:   "message_stop received, then the connection stays open: the reply is complete (the post-loop check exempts sawStop), not a stall",
			events: aTextBody, want: aTextWant,
		},
		{
			name: "two-text-blocks", provider: t5Anthropic, note: "text, tool_use, text: invariant (b) — stream and non-stream must agree on Content",
			events: []string{
				aMsgStart(`{"input_tokens":5,"output_tokens":1}`),
				aTextStart(0), aText(0, "before"), aBlockStop(0),
				aToolStart(1, "toolu_1", "list_tasks"), aBlockStop(1),
				aTextStart(2), aText(2, "after"), aBlockStop(2),
				aMsgDelta("tool_use", `{"output_tokens":4}`), aMsgStop(),
			},
			want: &t5View{Content: "before\nafter", Stop: "tool_use", In: 5, Out: 4, Miss: 5,
				ToolCalls: []t5TC{{ID: "toolu_1", Name: "list_tasks", Input: `{}`}}},
			nonStream: `{"model":"claude-opus-5","content":[{"type":"text","text":"before"},{"type":"tool_use","id":"toolu_1","name":"list_tasks","input":{}},{"type":"text","text":"after"}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":4}}`,
		},
		{
			name: "bare-json-error-body", provider: t5Anthropic, note: "a 200 whose body is a bare JSON error object (gateway), not SSE: classified like HTTP 529",
			events:     []string{aHTTPBody("overloaded_error") + "\n"},
			wantErr:    &t5Err{Kind: KindServerError, Temp: true, Status: 529},
			httpStatus: 529, httpBody: aHTTPBody("overloaded_error"),
		},
		{
			name: "large-tool-args-line", provider: t5Anthropic, only: []string{"whole", "split-json", "events-x3", "mixed-eol-7byte"},
			note: "one input_json_delta line of ~200KB (above bufio's 64KB default)",
			events: []string{
				aMsgStart(`{"input_tokens":1,"output_tokens":1}`),
				aToolStart(0, "toolu_w", "write_file"),
				aJSON(0, `{"content":"`+t5Big+`"}`), aBlockStop(0),
				aMsgDelta("tool_use", `{"output_tokens":2}`), aMsgStop(),
			},
			want: &t5View{Stop: "tool_use", In: 1, Out: 2, Miss: 1,
				ToolCalls: []t5TC{{ID: "toolu_w", Name: "write_file", Input: `{"content":"` + t5Big + `"}`}}},
		},

		// ---------------- OpenAI-compatible ----------------
		{
			name: "done-then-hang", provider: t5OpenAI, term: t5Hang, note: "[DONE] received, then the connection stays open: complete, not a stall",
			events: oTextBody, want: oTextWant,
		},
		{
			name: "bare-json-error-body", provider: t5OpenAI, note: "a 200 whose body is a bare JSON error object (gateway), not SSE: classified like HTTP 503",
			events:     []string{`{"error":{"message":"upstream overloaded","code":503}}` + "\n"},
			wantErr:    &t5Err{Kind: KindServerError, Temp: true, Status: 503},
			httpStatus: 503, httpBody: `{"error":{"message":"upstream overloaded","code":503}}`,
		},
		{
			name: "large-tool-args-line", provider: t5OpenAI, only: []string{"whole", "split-json", "events-x3", "mixed-eol-7byte"},
			note: "one tool-call delta line of ~200KB (above bufio.Scanner's 64KB default)",
			events: []string{
				oTC(`[{"index":0,"id":"call_w","type":"function","function":{"name":"write_file","arguments":` + t5JS(`{"content":"`+t5Big+`"}`) + `}}]`),
				oFinish("tool_calls"), oDone(),
			},
			want: &t5View{Stop: "tool_use", ToolCalls: []t5TC{{ID: "call_w", Name: "write_file", Input: `{"content":"` + t5Big + `"}`}}},
		},

		{
			name: "text-deltas", provider: t5OpenAI, note: "role chunk, text deltas incl. multi-byte runes, finish chunk, usage-only chunk, [DONE]",
			events: oTextBody, want: oTextWant, nonStream: oTextNonStream,
		},
		{
			name: "text-with-sse-lookalikes", provider: t5OpenAI, note: "text containing escaped newlines and \"data: [DONE]\" must not end the stream",
			events: []string{
				oText("line1\ndata: [DONE]\n\n"), oText(": not a comment\r\nend"), oFinish("stop"),
				oUsage(`{"prompt_tokens":4,"completion_tokens":9}`), oDone(),
			},
			want:      &t5View{Content: "line1\ndata: [DONE]\n\n: not a comment\r\nend", Stop: "stop", In: 4, Out: 9, Miss: 4},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":` + t5JS("line1\ndata: [DONE]\n\n: not a comment\r\nend") + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":9}}`,
		},
		{
			name: "noise-comments-unknown-empty", provider: t5OpenAI, note: "comments, event:/id: fields, empty data line, unknown chunk shapes are ignored",
			events: []string{
				": keepalive\n\n",
				oRole(),
				"event: message\nid: 1\n",
				oText("Hel"),
				"data:\n\n",
				oEv(`{}`),
				oEv(`{"object":"chat.completion.chunk","choices":[]}`),
				oEv(`{"type":"some_future_event","foo":1}`),
				oText("lo "),
				": keepalive\n\n",
				oText("世界"),
				oFinish("stop"), oUsage(oUsageFinal), oDone(),
			},
			want: oTextWant, nonStream: oTextNonStream,
		},
		{
			name: "data-without-space", provider: t5OpenAI, note: "\"data:\" without the optional space",
			events: []string{
				"data:" + `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n",
				"data:" + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}` + "\n\n",
				"data:[DONE]\n\n",
			},
			want: &t5View{Content: "ok", Stop: "stop", In: 2, Out: 1, Miss: 2},
		},
		{
			name: "usage-on-finish-chunk", provider: t5OpenAI, note: "usage carried on the finish chunk itself (DeepSeek style)",
			events: []string{
				oText("x"),
				oEv(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_cache_hit_tokens":2,"prompt_cache_miss_tokens":3}}`),
				oDone(),
			},
			want:      &t5View{Content: "x", Stop: "stop", In: 5, Out: 1, Hit: 2, Miss: 3},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_cache_hit_tokens":2,"prompt_cache_miss_tokens":3}}`,
		},
		{
			name: "usage-cumulative-every-chunk", provider: t5OpenAI, note: "servers that repeat cumulative usage on every chunk: the last one wins",
			events: []string{
				oEv(`{"choices":[{"index":0,"delta":{"content":"a"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`),
				oEv(`{"choices":[{"index":0,"delta":{"content":"b"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`),
				oEv(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`),
				oDone(),
			},
			want:      &t5View{Content: "ab", Stop: "stop", In: 10, Out: 3, Miss: 10},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ab"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
		},
		{
			name: "reasoning-then-text", provider: t5OpenAI, note: "reasoning_content deltas → ReasoningContent and reasoning callbacks; reasoning_tokens",
			events: []string{
				oRole(), oReason("Let me "), oReason("think."), oText("Answer"), oFinish("stop"),
				oUsage(`{"prompt_tokens":8,"completion_tokens":11,"completion_tokens_details":{"reasoning_tokens":5}}`), oDone(),
			},
			want:      &t5View{Content: "Answer", Reasoning: "Let me think.", Stop: "stop", In: 8, Out: 11, Miss: 8, ReasonTok: 5},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Answer","reasoning_content":"Let me think."},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":11,"completion_tokens_details":{"reasoning_tokens":5}}}`,
		},
		{
			name: "tool-args-split-by-index", provider: t5OpenAI, note: "one call: id/name on the first delta, arguments split mid-key and mid-string, merged by index",
			events: []string{
				oRole(),
				oTC(`[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]`),
				oTC(`[{"index":0,"function":{"arguments":"{\"pa"}}]`),
				oTC(`[{"index":0,"function":{"arguments":"th\": \"a.go"}}]`),
				oTC(`[{"index":0,"function":{"arguments":"\", \"limit\": 5}"}}]`),
				oFinish("tool_calls"), oUsage(`{"prompt_tokens":20,"completion_tokens":30}`), oDone(),
			},
			want: &t5View{Stop: "tool_use", In: 20, Out: 30, Miss: 20,
				ToolCalls: []t5TC{{ID: "call_1", Name: "read_file", Input: `{"limit":5,"path":"a.go"}`}}},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"a.go\", \"limit\": 5}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":30}}`,
		},
		{
			name: "tool-id-repeated-every-delta", provider: t5OpenAI, note: "servers repeating the same id (and name) on every delta: still one call",
			events: []string{
				oTC(`[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]`),
				oTC(`[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"\"a.go\"}"}}]`),
				oFinish("tool_calls"), oUsage(`{"prompt_tokens":2,"completion_tokens":3}`), oDone(),
			},
			want: &t5View{Stop: "tool_use", In: 2, Out: 3, Miss: 2,
				ToolCalls: []t5TC{{ID: "call_1", Name: "read_file", Input: `{"path":"a.go"}`}}},
		},
		{
			name: "two-tools-interleaved-by-index", provider: t5OpenAI, note: "two parallel calls at index 0/1 with interleaved argument deltas, ordered by start",
			events: []string{
				oTC(`[{"index":0,"id":"call_a","type":"function","function":{"name":"grep","arguments":""}}]`),
				oTC(`[{"index":1,"id":"call_b","type":"function","function":{"name":"glob","arguments":""}}]`),
				oTC(`[{"index":1,"function":{"arguments":"{\"pattern\":"}}]`),
				oTC(`[{"index":0,"function":{"arguments":"{\"q\":\"fo"}}]`),
				oTC(`[{"index":1,"function":{"arguments":"\"*.go\"}"}}]`),
				oTC(`[{"index":0,"function":{"arguments":"o\"}"}}]`),
				oFinish("tool_calls"), oUsage(`{"prompt_tokens":5,"completion_tokens":12}`), oDone(),
			},
			want: &t5View{Stop: "tool_use", In: 5, Out: 12, Miss: 5, ToolCalls: []t5TC{
				{ID: "call_a", Name: "grep", Input: `{"q":"foo"}`},
				{ID: "call_b", Name: "glob", Input: `{"pattern":"*.go"}`},
			}},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"grep","arguments":"{\"q\":\"foo\"}"}},{"id":"call_b","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":12}}`,
		},
		{
			name: "two-tools-in-one-delta", provider: t5OpenAI, note: "both calls in one tool_calls array",
			events: []string{
				oTC(`[{"index":0,"id":"call_a","type":"function","function":{"name":"grep","arguments":"{\"q\":\"foo\"}"}},{"index":1,"id":"call_b","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]`),
				oFinish("tool_calls"), oDone(),
			},
			want: &t5View{Stop: "tool_use", ToolCalls: []t5TC{
				{ID: "call_a", Name: "grep", Input: `{"q":"foo"}`},
				{ID: "call_b", Name: "glob", Input: `{"pattern":"*.go"}`},
			}},
		},
		{
			name: "two-tools-same-index-new-id", provider: t5OpenAI, note: "servers sending every parallel call whole at index 0: a new id starts a new call (merge by id)",
			events: []string{
				oTC(`[{"index":0,"id":"call_a","type":"function","function":{"name":"grep","arguments":"{\"q\":\"foo\"}"}}]`),
				oTC(`[{"index":0,"id":"call_b","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]`),
				oFinish("tool_calls"), oDone(),
			},
			want: &t5View{Stop: "tool_use", ToolCalls: []t5TC{
				{ID: "call_a", Name: "grep", Input: `{"q":"foo"}`},
				{ID: "call_b", Name: "glob", Input: `{"pattern":"*.go"}`},
			}},
		},
		{
			name: "tool-without-id", provider: t5OpenAI, anyIDs: true, note: "a call without id gets a generated one (both paths)",
			events: []string{
				oTC(`[{"index":0,"type":"function","function":{"name":"list_tasks","arguments":"{}"}}]`),
				oFinish("tool_calls"), oDone(),
			},
			want:      &t5View{Stop: "tool_use", ToolCalls: []t5TC{{ID: "<generated>", Name: "list_tasks", Input: `{}`}}},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"list_tasks","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "tool-no-args", provider: t5OpenAI, note: "a call streaming no argument text is a complete call with empty input",
			events: []string{
				oTC(`[{"index":0,"id":"call_n","type":"function","function":{"name":"list_tasks"}}]`),
				oFinish("tool_calls"), oDone(),
			},
			want: &t5View{Stop: "tool_use", ToolCalls: []t5TC{{ID: "call_n", Name: "list_tasks", Input: `{}`}}},
		},
		{
			name: "text-and-tool", provider: t5OpenAI, note: "text followed by a tool call in the same reply",
			events: []string{
				oText("Let me look."),
				oTC(`[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]`),
				oFinish("tool_calls"), oUsage(`{"prompt_tokens":1,"completion_tokens":2}`), oDone(),
			},
			want: &t5View{Content: "Let me look.", Stop: "tool_use", In: 1, Out: 2, Miss: 1,
				ToolCalls: []t5TC{{ID: "call_1", Name: "read_file", Input: `{"path":"a.go"}`}}},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Let me look.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		},
		{
			name: "finish-length", provider: t5OpenAI, note: "finish_reason length passes through",
			events:    []string{oText("trunc"), oFinish("length"), oUsage(`{"prompt_tokens":3,"completion_tokens":64}`), oDone()},
			want:      &t5View{Content: "trunc", Stop: "length", In: 3, Out: 64, Miss: 3},
			nonStream: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"trunc"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":64}}`,
		},
		{
			name: "done-without-finish", provider: t5OpenAI, note: "[DONE] without any finish_reason is a complete reply with stop",
			events: []string{oText("ok"), oDone()},
			want:   &t5View{Content: "ok", Stop: "stop"},
		},
		{
			name: "finish-without-done", provider: t5OpenAI, note: "finish_reason seen, [DONE] lost: accepted",
			events: []string{oText("ok"), oFinish("stop"), oUsage(`{"prompt_tokens":1,"completion_tokens":1}`)},
			want:   &t5View{Content: "ok", Stop: "stop", In: 1, Out: 1, Miss: 1},
		},
		{
			name: "insufficient-system-resource", provider: t5OpenAI, note: "DeepSeek's aborted inference is a retryable 503 on both paths",
			events:     []string{oText("par"), oFinish(finishInsufficientResource), oDone()},
			wantErr:    &t5Err{Kind: KindServerError, Temp: true, Status: http.StatusServiceUnavailable},
			nonStream:  `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"par"},"finish_reason":"insufficient_system_resource"}]}`,
			httpStatus: http.StatusServiceUnavailable, httpBody: `{"error":{"message":"busy"}}`,
		},
		{
			name: "close-early", provider: t5OpenAI, note: "server closes cleanly mid-reply: error, not stop",
			events:  []string{oRole(), oText("half")},
			wantErr: &t5Err{Kind: KindTransport, Temp: true},
		},
		{
			name: "close-before-any-event", provider: t5OpenAI, note: "200 header then empty body",
			events:  []string{},
			wantErr: &t5Err{Kind: KindTransport, Temp: true},
		},
		{
			name: "abort-mid-stream", provider: t5OpenAI, term: t5Abort, note: "connection dropped mid-stream",
			events:  []string{oRole(), oText("half")},
			wantErr: &t5Err{Kind: KindTransport, Temp: true},
		},
		{
			name: "hang-watchdog", provider: t5OpenAI, term: t5Hang, note: "server stops sending: stall",
			events:  []string{oRole(), oText("half")},
			wantErr: &t5Err{Kind: KindTimeout, Temp: true},
		},
		{
			name: "ctx-cancel", provider: t5OpenAI, term: t5Cancel, note: "caller cancels mid-stream",
			events:  []string{oRole(), oText("half")},
			wantErr: &t5Err{Kind: KindCanceled},
		},
	}

	// In-stream errors, Anthropic: every error type the API documents, each
	// after some text; classification must equal the HTTP status's (c).
	for _, e := range []struct {
		typ    string
		status int
		kind   ErrorKind
	}{
		{"overloaded_error", 529, KindServerError},
		{"api_error", http.StatusInternalServerError, KindServerError},
		{"rate_limit_error", http.StatusTooManyRequests, KindRateLimit},
		{"authentication_error", http.StatusUnauthorized, KindAuth},
		{"permission_error", http.StatusForbidden, KindAuth},
		{"invalid_request_error", http.StatusBadRequest, KindBadRequest},
		{"not_found_error", http.StatusNotFound, KindUnknown},
		{"request_too_large", http.StatusRequestEntityTooLarge, KindContextLength},
	} {
		cases = append(cases, t5Case{
			name: "stream-error-" + e.typ, provider: t5Anthropic, note: "in-stream error event classifies like HTTP " + fmt.Sprint(e.status),
			events: []string{
				aMsgStart(`{"input_tokens":3,"output_tokens":1}`), aTextStart(0), aText(0, "par"),
				aError(e.typ),
			},
			wantErr:    &t5Err{Kind: e.kind, Temp: e.status >= 500, Status: e.status},
			httpStatus: e.status, httpBody: aHTTPBody(e.typ),
		})
	}
	cases = append(cases, t5Case{
		name: "stream-error-unknown-type", provider: t5Anthropic, note: "unknown error type: an error without an invented status",
		events:  []string{aMsgStart(`{"input_tokens":3,"output_tokens":1}`), aError("weird_error")},
		wantErr: &t5Err{Kind: KindUnknown},
	})

	// In-stream errors, OpenAI-compatible: numeric code, numeric string code,
	// type strings, and a non-numeric string code.
	for _, e := range []struct {
		name   string
		obj    string
		status int
		kind   ErrorKind
	}{
		{"code-503", `{"message":"upstream overloaded","type":"server_error","code":503}`, 503, KindServerError},
		{"code-500", `{"message":"boom","code":500}`, 500, KindServerError},
		{"code-502-string", `{"message":"bad gateway","code":"502"}`, 502, KindServerError},
		{"code-429-string", `{"message":"slow down","code":"429"}`, 429, KindRateLimit},
		{"code-401", `{"message":"bad key","code":401}`, 401, KindAuth},
		{"code-403", `{"message":"denied","code":403}`, 403, KindAuth},
		{"code-400", `{"message":"bad","type":"invalid_request_error","code":400}`, 400, KindBadRequest},
		{"code-404", `{"message":"no model","code":404}`, 404, KindUnknown},
		{"code-413", `{"message":"too big","code":413}`, 413, KindContextLength},
		{"type-rate_limit_exceeded", `{"message":"slow down","type":"rate_limit_exceeded","code":null}`, 429, KindRateLimit},
		{"type-server_error", `{"message":"broken","type":"server_error"}`, 500, KindServerError},
		{"type-authentication_error", `{"message":"bad key","type":"authentication_error"}`, 401, KindAuth},
		{"type-overloaded_error", `{"message":"busy","type":"overloaded_error"}`, 529, KindServerError},
	} {
		cases = append(cases, t5Case{
			name: "stream-error-" + e.name, provider: t5OpenAI, note: "in-stream error chunk classifies like HTTP " + fmt.Sprint(e.status),
			events:     []string{oRole(), oText("par"), oErr(e.obj), oDone()},
			wantErr:    &t5Err{Kind: e.kind, Temp: e.status >= 500, Status: e.status},
			httpStatus: e.status, httpBody: `{"error":` + e.obj + `}`,
		})
	}
	cases = append(cases, t5Case{
		name: "stream-error-code-insufficient_quota", provider: t5OpenAI, note: "non-numeric string code: error without an invented status",
		events:  []string{oText("par"), oErr(`{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}`), oDone()},
		wantErr: &t5Err{Kind: KindUnknown},
	})
	return cases
}
