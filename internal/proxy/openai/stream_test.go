package openai

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeStreamErrorFrame(t *testing.T) {
	const errorObject = `"error":{"message":"servers overloaded","type":"server_error","code":"server_is_overloaded"}`
	tests := []struct {
		name string
		data string
	}{
		{name: "choices absent", data: `{` + errorObject + `}`},
		{name: "choices null", data: `{` + errorObject + `,"choices":null}`},
		{name: "choices empty", data: `{` + errorObject + `,"choices":[]}`},
		{name: "choices empty with identity", data: `{"id":"chatcmpl-error","model":"up",` + errorObject + `,"choices":[]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out []ir.StreamEvent
			err := DecodeStream(strings.NewReader("data: "+tc.data+"\n\n"), func(ev ir.StreamEvent) error {
				out = append(out, ev)
				return nil
			})
			sf, ok := ir.AsStreamFailure(err)
			if !ok {
				t.Fatalf("want StreamFailure, got %v", err)
			}
			if sf.Type != "server_error" {
				t.Errorf("type = %q", sf.Type)
			}
			if sf.Code != "server_is_overloaded" {
				t.Errorf("code = %q", sf.Code)
			}
			if !strings.Contains(sf.Message, "overloaded") {
				t.Errorf("message = %q", sf.Message)
			}
			for _, ev := range out {
				if ev.Kind == ir.EventFinish || ev.Kind == ir.EventMessageStart {
					t.Fatalf("unexpected event %+v", ev)
				}
			}
		})
	}
}

func TestDecodeStreamErrorScalarNormalization(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		wantCode    string
		wantType    string
		wantMessage string
	}{
		{
			name:        "numeric code",
			data:        `{"error":{"message":"servers overloaded","type":"server_error","code":429}}`,
			wantCode:    "429",
			wantType:    "server_error",
			wantMessage: "servers overloaded",
		},
		{
			name:        "null code",
			data:        `{"error":{"message":"servers overloaded","type":"server_error","code":null}}`,
			wantCode:    "",
			wantType:    "server_error",
			wantMessage: "servers overloaded",
		},
		{
			name:        "boolean code",
			data:        `{"error":{"message":"servers overloaded","type":"server_error","code":true}}`,
			wantCode:    "",
			wantType:    "server_error",
			wantMessage: "servers overloaded",
		},
		{
			name:        "object code",
			data:        `{"error":{"message":"servers overloaded","type":"server_error","code":{"n":1}}}`,
			wantCode:    "",
			wantType:    "server_error",
			wantMessage: "servers overloaded",
		},
		{
			name:        "array code",
			data:        `{"error":{"message":"servers overloaded","type":"server_error","code":[429]}}`,
			wantCode:    "",
			wantType:    "server_error",
			wantMessage: "servers overloaded",
		},
		{
			name:        "string code control",
			data:        `{"error":{"message":"servers overloaded","type":"server_error","code":"server_is_overloaded"}}`,
			wantCode:    "server_is_overloaded",
			wantType:    "server_error",
			wantMessage: "servers overloaded",
		},
		{
			name:        "empty metadata fallback",
			data:        `{"error":{}}`,
			wantMessage: "upstream stream failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out []ir.StreamEvent
			err := DecodeStream(strings.NewReader("data: "+tc.data+"\n\n"), func(ev ir.StreamEvent) error {
				out = append(out, ev)
				return nil
			})
			sf, ok := ir.AsStreamFailure(err)
			if !ok {
				t.Fatalf("want StreamFailure, got %v", err)
			}
			if sf.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", sf.Code, tc.wantCode)
			}
			if sf.Type != tc.wantType {
				t.Errorf("type = %q, want %q", sf.Type, tc.wantType)
			}
			if sf.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", sf.Message, tc.wantMessage)
			}
			for _, ev := range out {
				if ev.Kind == ir.EventFinish || ev.Kind == ir.EventMessageStart {
					t.Fatalf("unexpected event %+v", ev)
				}
			}
		})
	}
}

func TestDecodeStreamNumericCodeWithIdentityNoLifecycle(t *testing.T) {
	body := `data: {"id":"chatcmpl-error","model":"up","choices":[],"error":{"message":"servers overloaded","type":"server_error","code":429}}` + "\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "429" {
		t.Errorf("code = %q", sf.Code)
	}
	if sf.Type != "server_error" {
		t.Errorf("type = %q", sf.Type)
	}
	for _, ev := range out {
		if ev.Kind == ir.EventFinish || ev.Kind == ir.EventMessageStart {
			t.Fatalf("unexpected event %+v", ev)
		}
	}
}

func TestNormalizeErrorScalar(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string", raw: `"server_is_overloaded"`, want: "server_is_overloaded"},
		{name: "number", raw: `429`, want: "429"},
		{name: "negative number", raw: `-1`, want: "-1"},
		{name: "null", raw: `null`, want: ""},
		{name: "boolean", raw: `true`, want: ""},
		{name: "object", raw: `{"n":1}`, want: ""},
		{name: "array", raw: `[429]`, want: ""},
		{name: "empty", raw: ``, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeErrorScalar([]byte(tc.raw)); got != tc.want {
				t.Fatalf("normalizeErrorScalar(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestDecodeStreamUsageOnlyTrailerNoFabrication(t *testing.T) {
	trailer := "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"model\":\"up\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0}}\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(trailer), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("events = %+v, want none", out)
	}

	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"up\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	out = nil
	err = DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].Kind != ir.EventMessageStart || out[1].Kind != ir.EventTextDelta || out[1].Text != "ok" || out[2].Kind != ir.EventFinish {
		t.Fatalf("events = %+v", out)
	}
	if out[2].InputTokens != 10 || out[2].OutputTokens != 2 {
		t.Fatalf("usage = in %d/out %d, want 10/2", out[2].InputTokens, out[2].OutputTokens)
	}
}

func TestDecodeStreamChunkWithChoices(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].Kind != ir.EventMessageStart || out[1].Kind != ir.EventTextDelta || out[1].Text != "ok" || out[2].Kind != ir.EventFinish {
		t.Fatalf("events = %+v", out)
	}
}

func sseData(chunks ...string) string {
	var b strings.Builder
	for _, chunk := range chunks {
		b.WriteString("data: ")
		b.WriteString(chunk)
		b.WriteString("\n\n")
	}
	return b.String()
}

func decodeStreamEvents(t *testing.T, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	return out, err
}

func TestDecodeStreamMetadataOnlyEmitsNothing(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{
			name: "role only eof",
			chunks: []string{
				`{"id":"chatcmpl-role","model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			},
		},
		{
			name: "role plus usage eof",
			chunks: []string{
				`{"id":"chatcmpl-role","model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`,
			},
		},
		{
			name: "role only done",
			chunks: []string{
				`{"id":"chatcmpl-role","model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`[DONE]`,
			},
		},
		{
			name: "usage only done",
			chunks: []string{
				`{"id":"chatcmpl-x","model":"up","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0}}`,
				`[DONE]`,
			},
		},
		{
			name:   "done only",
			chunks: []string{`[DONE]`},
		},
		{
			name: "empty finish reason is not evidence",
			chunks: []string{
				`{"id":"chatcmpl-role","model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":""}]}`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := decodeStreamEvents(t, sseData(tc.chunks...))
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 0 {
				t.Fatalf("events = %+v, want none", out)
			}
		})
	}
}

func TestDecodeStreamRoleThenExplicitFinishKeepsMetadata(t *testing.T) {
	body := sseData(
		`{"id":"chatcmpl-empty","model":"saved-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		`[DONE]`,
	)
	out, err := decodeStreamEvents(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Kind != ir.EventMessageStart || out[1].Kind != ir.EventFinish {
		t.Fatalf("events = %+v, want start then finish", out)
	}
	if out[0].ID != "chatcmpl-empty" || out[0].Model != "saved-model" {
		t.Fatalf("start metadata = %q/%q", out[0].ID, out[0].Model)
	}
	if out[1].StopReason != ir.StopMaxTokens {
		t.Fatalf("stop = %q, want max_tokens", out[1].StopReason)
	}
}

func TestDecodeStreamTrailingUsageAfterEmptyFinish(t *testing.T) {
	body := sseData(
		`{"id":"chatcmpl-empty","model":"saved-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":7,"cache_write_tokens":2}}}`,
		`[DONE]`,
	)
	out, err := decodeStreamEvents(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Kind != ir.EventMessageStart || out[1].Kind != ir.EventFinish {
		t.Fatalf("events = %+v, want one deferred finish", out)
	}
	finish := out[1]
	if finish.InputTokens != 11 || finish.OutputTokens != 0 || finish.CacheReadTokens != 7 || finish.CacheWriteTokens != 2 {
		t.Fatalf("usage = %+v", finish)
	}
	if finish.StopReason != ir.StopEndTurn {
		t.Fatalf("stop = %q", finish.StopReason)
	}
}

func TestDecodeStreamRolePreambleThenOutputKeepsSavedMetadata(t *testing.T) {
	tests := []struct {
		name    string
		chunks  []string
		want    []ir.StreamEventKind
		text    string
		toolID  string
		toolArg string
		stop    ir.StopReason
	}{
		{
			name: "text",
			chunks: []string{
				`{"id":"chatcmpl-1","model":"saved","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			want: []ir.StreamEventKind{ir.EventMessageStart, ir.EventTextDelta, ir.EventFinish},
			text: "ok",
			stop: ir.StopEndTurn,
		},
		{
			name: "whitespace is content",
			chunks: []string{
				`{"id":"chatcmpl-1","model":"saved","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"content":" "},"finish_reason":null}]}`,
			},
			want: []ir.StreamEventKind{ir.EventMessageStart, ir.EventTextDelta, ir.EventFinish},
			text: " ",
			stop: ir.StopEndTurn,
		},
		{
			name: "reasoning",
			chunks: []string{
				`{"id":"chatcmpl-1","model":"saved","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"reasoning_content":"chain"},"finish_reason":null}]}`,
			},
			want: []ir.StreamEventKind{ir.EventMessageStart, ir.EventReasoningDelta, ir.EventFinish},
			text: "chain",
			stop: ir.StopEndTurn,
		},
		{
			name: "tool fragments",
			chunks: []string{
				`{"id":"chatcmpl-1","model":"saved","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_9","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"city\":\"paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
			},
			want:    []ir.StreamEventKind{ir.EventMessageStart, ir.EventToolCallStart, ir.EventToolCallDelta, ir.EventFinish},
			toolID:  "call_9",
			toolArg: `{"city":"paris"}`,
			stop:    ir.StopToolUse,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := decodeStreamEvents(t, sseData(tc.chunks...))
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != len(tc.want) {
				t.Fatalf("events = %+v", out)
			}
			for i, kind := range tc.want {
				if out[i].Kind != kind {
					t.Fatalf("event %d = %+v, want %v", i, out[i], kind)
				}
			}
			if out[0].ID != "chatcmpl-1" || out[0].Model != "saved" {
				t.Fatalf("start metadata = %q/%q", out[0].ID, out[0].Model)
			}
			switch out[1].Kind {
			case ir.EventTextDelta, ir.EventReasoningDelta:
				if out[1].Text != tc.text {
					t.Fatalf("text = %q, want %q", out[1].Text, tc.text)
				}
			case ir.EventToolCallStart:
				if out[1].Index != 1 || out[1].ToolID != tc.toolID || out[1].ToolName != "get_weather" {
					t.Fatalf("tool start = %+v", out[1])
				}
				if out[2].Kind != ir.EventToolCallDelta || out[2].Index != 1 || out[2].ArgsFrag != tc.toolArg {
					t.Fatalf("tool delta = %+v", out[2])
				}
			}
			finish := out[len(out)-1]
			if finish.Kind != ir.EventFinish || finish.StopReason != tc.stop {
				t.Fatalf("finish = %+v, want stop %q", finish, tc.stop)
			}
		})
	}
}

func TestDecodeStreamLatestMetadataBeforeStart(t *testing.T) {
	body := sseData(
		`{"id":"chatcmpl-old","model":"old-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-new","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"model":"","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
	)
	out, err := decodeStreamEvents(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 1 || out[0].Kind != ir.EventMessageStart {
		t.Fatalf("events = %+v", out)
	}
	if out[0].ID != "chatcmpl-new" || out[0].Model != "old-model" {
		t.Fatalf("metadata = %q/%q, want latest non-empty id and retained model", out[0].ID, out[0].Model)
	}
}

func TestDecodeStreamRoleThenErrorStaysUnstarted(t *testing.T) {
	body := sseData(
		`{"id":"chatcmpl-role","model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"error":{"message":"servers overloaded","type":"server_error","code":"server_is_overloaded"}}`,
	)
	out, err := decodeStreamEvents(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Message != "servers overloaded" {
		t.Fatalf("message = %q", sf.Message)
	}
	if len(out) != 0 {
		t.Fatalf("events = %+v, want none before error", out)
	}
}
