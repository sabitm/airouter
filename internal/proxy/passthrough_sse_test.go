package proxy

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestClassifyPassthroughOpenAIChat(t *testing.T) {
	cases := []struct {
		name string
		data string
		want passthroughClass
		fail bool
		msg  string
		typ  string
		code string
	}{
		{name: "done", data: "[DONE]", want: passTerminal},
		{name: "top-level error", data: `{"error":{"message":"servers overloaded","type":"server_error","code":"server_is_overloaded"}}`, want: passFailure, fail: true, msg: "servers overloaded", typ: "server_error", code: "server_is_overloaded"},
		{name: "error empty choices", data: `{"id":"chatcmpl-error","choices":[],"error":{"message":"boom","type":"server_error","code":"x"}}`, want: passFailure, fail: true, msg: "boom", typ: "server_error", code: "x"},
		{name: "error null choices", data: `{"error":{"message":"boom","type":"api_error"},"choices":null}`, want: passFailure, fail: true, msg: "boom", typ: "api_error"},
		{name: "text delta", data: `{"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`, want: passVisible},
		{name: "tool delta", data: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"fn"}}]}}]}`, want: passVisible},
		{name: "role only", data: `{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, want: passLifecycle},
		{name: "empty content", data: `{"choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}`, want: passLifecycle},
		{name: "usage only", data: `{"id":"c1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`, want: passLifecycle},
		{name: "finish stop", data: `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, want: passTerminal},
		{name: "content and finish", data: `{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`, want: passVisible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, fail := classifyPassthroughEvent("oai-chat", sse.Event{Data: []byte(tc.data)})
			if class != tc.want {
				t.Fatalf("class = %v, want %v (fail=%v)", class, tc.want, fail)
			}
			if tc.fail {
				assertFailure(t, fail, tc.msg, tc.typ, tc.code)
			} else if fail != nil {
				t.Fatalf("unexpected failure %+v", fail)
			}
		})
	}
}

func TestClassifyPassthroughResponses(t *testing.T) {
	cases := []struct {
		name string
		ev   sse.Event
		want passthroughClass
		fail bool
		msg  string
		typ  string
		code string
	}{
		{name: "created", ev: named("response.created", `{"type":"response.created","response":{"id":"r1","status":"in_progress"}}`), want: passLifecycle},
		{name: "in_progress", ev: named("response.in_progress", `{"type":"response.in_progress","response":{"status":"in_progress"}}`), want: passLifecycle},
		{name: "message item added", ev: named("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"message","role":"assistant","content":[]}}`), want: passLifecycle},
		{name: "function item added", ev: named("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","name":"fn"}}`), want: passVisible},
		{name: "text delta", ev: named("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hello"}`), want: passVisible},
		{name: "empty text delta", ev: named("response.output_text.delta", `{"type":"response.output_text.delta","delta":""}`), want: passLifecycle},
		{name: "arg delta", ev: named("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","delta":"{"}`), want: passVisible},
		{name: "completed", ev: named("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`), want: passTerminal},
		{name: "incomplete", ev: named("response.incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`), want: passTerminal},
		{name: "type error", ev: named("error", `{"type":"error","error":{"type":"server_error","code":"boom","message":"mid-stream boom"}}`), want: passFailure, fail: true, msg: "mid-stream boom", typ: "server_error", code: "boom"},
		{name: "response.failed", ev: named("response.failed", `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"overloaded"}}}`), want: passFailure, fail: true, msg: "overloaded", code: "server_is_overloaded"},
		{name: "failed status", ev: named("response.completed", `{"type":"response.completed","response":{"status":"failed","error":{"message":"nope","type":"api_error"}}}`), want: passFailure, fail: true, msg: "nope", typ: "api_error"},
		{name: "unknown non-error", ev: named("response.output_text.done", `{"type":"response.output_text.done","text":"Hello"}`), want: passVisible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, fail := classifyPassthroughEvent("oai-responses", tc.ev)
			if class != tc.want {
				t.Fatalf("class = %v, want %v (fail=%v)", class, tc.want, fail)
			}
			if tc.fail {
				assertFailure(t, fail, tc.msg, tc.typ, tc.code)
			} else if fail != nil {
				t.Fatalf("unexpected failure %+v", fail)
			}
		})
	}
}

func TestClassifyPassthroughAnthropic(t *testing.T) {
	cases := []struct {
		name string
		ev   sse.Event
		want passthroughClass
		fail bool
		msg  string
		typ  string
	}{
		{name: "message_start", ev: named("message_start", `{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":3}}}`), want: passLifecycle},
		{name: "empty text start", ev: named("content_block_start", `{"type":"content_block_start","content_block":{"type":"text","text":""}}`), want: passLifecycle},
		{name: "tool start", ev: named("content_block_start", `{"type":"content_block_start","content_block":{"type":"tool_use","id":"t1","name":"fn"}}`), want: passVisible},
		{name: "text delta", ev: named("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`), want: passVisible},
		{name: "empty text delta", ev: named("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`), want: passLifecycle},
		{name: "tool delta", ev: named("content_block_delta", `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}`), want: passVisible},
		{name: "block stop", ev: named("content_block_stop", `{"type":"content_block_stop","index":0}`), want: passLifecycle},
		{name: "message_delta", ev: named("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`), want: passLifecycle},
		{name: "message_stop", ev: named("message_stop", `{"type":"message_stop"}`), want: passTerminal},
		{name: "ping", ev: named("ping", `{"type":"ping"}`), want: passLifecycle},
		{name: "event error", ev: named("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), want: passFailure, fail: true, msg: "Overloaded", typ: "overloaded_error"},
		{name: "type error no name", ev: sse.Event{Data: []byte(`{"type":"error","error":{"type":"api_error","message":"nope"}}`)}, want: passFailure, fail: true, msg: "nope", typ: "api_error"},
		{name: "unknown non-error", ev: named("content_block_something", `{"type":"content_block_something"}`), want: passVisible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, fail := classifyPassthroughEvent("anth-msg", tc.ev)
			if class != tc.want {
				t.Fatalf("class = %v, want %v (fail=%v)", class, tc.want, fail)
			}
			if tc.fail {
				assertFailure(t, fail, tc.msg, tc.typ, "")
			} else if fail != nil {
				t.Fatalf("unexpected failure %+v", fail)
			}
		})
	}
}

func TestClassifyPassthroughUnknownCodecCommits(t *testing.T) {
	class, fail := classifyPassthroughEvent("unknown", sse.Event{Name: "x", Data: []byte(`{}`)})
	if class != passVisible || fail != nil {
		t.Fatalf("class=%v fail=%v, want visible", class, fail)
	}
}

func named(name, data string) sse.Event {
	return sse.Event{Name: name, Data: []byte(data)}
}

func assertFailure(t *testing.T, fail *ir.StreamFailure, msg, typ, code string) {
	t.Helper()
	if fail == nil {
		t.Fatal("want StreamFailure")
	}
	if msg != "" && !strings.Contains(fail.Message, msg) {
		t.Errorf("message = %q, want contain %q", fail.Message, msg)
	}
	if typ != "" && fail.Type != typ {
		t.Errorf("type = %q, want %q", fail.Type, typ)
	}
	if code != "" && fail.Code != code {
		t.Errorf("code = %q, want %q", fail.Code, code)
	}
}
