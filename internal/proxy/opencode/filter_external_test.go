// Package opencode_test exercises DecodeStream through the real backend
// decoders. It lives in an external test package because the codec packages
// import the thinking package, which imports opencode.
package opencode_test

import (
	"io"
	"strings"
	"testing"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/responses"
)

func newFilterMapping(t *testing.T, sent map[string]string, decoys ...string) opencode.ToolMap {
	t.Helper()
	m := opencode.ToolMap{SentToCaller: sent, Decoys: map[string]bool{}}
	for _, d := range decoys {
		m.Decoys[d] = true
	}
	return m
}

func sseEvents(events ...string) string {
	return strings.Join(events, "\n\n") + "\n\n"
}

func runFilter(t *testing.T, decode func(io.Reader, func(ir.StreamEvent) error) error, mapping opencode.ToolMap, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	var events []ir.StreamEvent
	err := opencode.DecodeStream(decode, mapping)(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

func toolArgs(events []ir.StreamEvent, index int) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta && ev.Index == index {
			b.WriteString(ev.ArgsFrag)
		}
	}
	return b.String()
}

func TestOpenAIFilterHoldsIDAndArgsUntilName(t *testing.T) {
	mapping := newFilterMapping(t, map[string]string{"read": "Read"}, "bash")
	body := sseEvents(
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1"}]},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	events, err := runFilter(t, openai.DecodeStream, mapping, body)
	if err != nil {
		t.Fatal(err)
	}
	var starts []ir.StreamEvent
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			starts = append(starts, ev)
		}
		if ev.ToolName == "read" {
			t.Fatalf("wire name leaked before restore: %+v", events)
		}
	}
	if len(starts) != 1 || starts[0].ToolID != "call_1" || starts[0].ToolName != "Read" {
		t.Fatalf("starts = %+v", starts)
	}
	if got := toolArgs(events, 0); got != `{"p":1}` {
		t.Fatalf("args = %q events=%+v", got, events)
	}
	// The held fragment must follow the accepted start, not precede it.
	startAt, firstDelta := -1, -1
	for i, ev := range events {
		if ev.Kind == ir.EventToolCallStart && ev.Index == 0 && startAt == -1 {
			startAt = i
		}
		if ev.Kind == ir.EventToolCallDelta && ev.Index == 0 && firstDelta == -1 {
			firstDelta = i
		}
	}
	if startAt == -1 || firstDelta == -1 || startAt > firstDelta {
		t.Fatalf("start/delta order wrong: %+v", events)
	}
}

func TestAnthropicFilterHoldsArgsBeforeIdentity(t *testing.T) {
	mapping := newFilterMapping(t, map[string]string{"read": "Read"}, "bash")
	body := sseEvents(
		"event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"p\":"}}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read"}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		"event: message_stop\ndata: "+`{"type":"message_stop"}`,
	)
	events, err := runFilter(t, anthropic.DecodeStream, mapping, body)
	if err != nil {
		t.Fatal(err)
	}
	var starts []ir.StreamEvent
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			starts = append(starts, ev)
		}
		if ev.ToolName == "read" {
			t.Fatalf("wire name leaked: %+v", events)
		}
	}
	if len(starts) != 1 || starts[0].ToolID != "toolu_1" || starts[0].ToolName != "Read" {
		t.Fatalf("starts = %+v", starts)
	}
	if got := toolArgs(events, 1); got != `{"p":1}` {
		t.Fatalf("args = %q events=%+v", got, events)
	}
}

func TestResponsesFilterRestoresName(t *testing.T) {
	mapping := newFilterMapping(t, map[string]string{"grep": "Grep"}, "bash")
	body := sseEvents(
		"event: response.created\ndata: "+`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		"event: response.function_call_arguments.delta\ndata: "+`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"p\":"}`,
		"event: response.output_item.added\ndata: "+`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"grep"}}`,
		"event: response.function_call_arguments.delta\ndata: "+`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"1}"}`,
		"event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2},"output":[{"type":"function_call","call_id":"call_1","name":"grep","arguments":"{\"p\":1}"}]}}`,
	)
	events, err := runFilter(t, responses.DecodeStream, mapping, body)
	if err != nil {
		t.Fatal(err)
	}
	var start *ir.StreamEvent
	for i := range events {
		switch {
		case events[i].Kind == ir.EventToolCallStart:
			start = &events[i]
		case events[i].ToolName == "grep":
			t.Fatalf("wire name leaked: %+v", events)
		}
	}
	if start == nil || start.ToolName != "Grep" {
		t.Fatalf("start = %+v events=%+v", start, events)
	}
	if got := toolArgs(events, start.Index); got != `{"p":1}` {
		t.Fatalf("args = %q events=%+v", got, events)
	}
}

func TestFilterInterleavedIndicesDoesNotLeakDecoy(t *testing.T) {
	mapping := newFilterMapping(t, map[string]string{"read": "Read"}, "bash")
	body := sseEvents(
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"a"}}]},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_bad"}]},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"secret"}}]},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"b"}}]},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"name":"bash"}}]},"finish_reason":null}]}`,
		`data: [DONE]`,
	)
	events, err := runFilter(t, openai.DecodeStream, mapping, body)
	if err == nil || !strings.Contains(err.Error(), "bash") {
		t.Fatalf("err = %v", err)
	}
	for _, ev := range events {
		if ev.ToolID == "call_bad" || ev.ArgsFrag == "secret" || ev.ToolName == "bash" {
			t.Fatalf("decoy leaked: %+v", events)
		}
	}
	if got := toolArgs(events, 0); got != "ab" {
		t.Fatalf("accepted args = %q events=%+v", got, events)
	}
}

func TestOpenAIFilterUnknownToolsPassThrough(t *testing.T) {
	mapping := newFilterMapping(t, map[string]string{"read": "Read"}, "bash")
	body := sseEvents(
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"shell","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: [DONE]`,
	)
	events, err := runFilter(t, openai.DecodeStream, mapping, body)
	if err != nil {
		t.Fatal(err)
	}
	var start *ir.StreamEvent
	for i := range events {
		if events[i].Kind == ir.EventToolCallStart {
			start = &events[i]
		}
	}
	if start == nil || start.ToolName != "shell" {
		t.Fatalf("start = %+v", start)
	}
}

// TestAnthropicFilterBareToolUseStartRejects runs the real Anthropic decoder
// over malformed tool_use blocks that carry no ID and no name. The production
// decoder emits a bare EventToolCallStart for them; the wrapper must reject at
// finish instead of dropping the call, and no held payload may leak.
func TestAnthropicFilterBareToolUseStartRejects(t *testing.T) {
	mapping := newFilterMapping(t, map[string]string{"read": "Read"}, "bash")
	messageStart := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`
	bareStart := "event: content_block_start\ndata: " + `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"","name":""}}`
	argsDelta := "event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"secret\":1}"}}`
	cases := []struct {
		name string
		body string
	}{
		{"bare start then finish", sseEvents(
			messageStart,
			bareStart,
			"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
			"event: message_stop\ndata: "+`{"type":"message_stop"}`,
		)},
		{"bare start with args and clean eof", sseEvents(
			messageStart,
			bareStart,
			argsDelta,
			"event: message_stop\ndata: "+`{"type":"message_stop"}`,
		)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, err := runFilter(t, anthropic.DecodeStream, mapping, tc.body)
			if err == nil || !strings.Contains(err.Error(), "without a name") {
				t.Fatalf("malformed tool_use accepted: %v", err)
			}
			for _, ev := range events {
				if ev.Kind == ir.EventFinish || ev.ArgsFrag != "" || ev.ToolID != "" || ev.ToolName != "" {
					t.Fatalf("unvalidated tool data forwarded: %+v", events)
				}
			}
		})
	}
}

// TestAnthropicFilterBareStartThenRealToolSucceeds proves the same decoder
// accepts a stream whose tool call only later receives its identity.
func TestAnthropicFilterBareStartThenRealToolSucceeds(t *testing.T) {
	mapping := newFilterMapping(t, map[string]string{"read": "Read"}, "bash")
	body := sseEvents(
		"event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"","name":""}}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read"}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"p\":1}"}}`,
		"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		"event: message_stop\ndata: "+`{"type":"message_stop"}`,
	)
	events, err := runFilter(t, anthropic.DecodeStream, mapping, body)
	if err != nil {
		t.Fatal(err)
	}
	var starts []ir.StreamEvent
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			starts = append(starts, ev)
		}
		if ev.ToolName == "read" {
			t.Fatalf("wire name leaked: %+v", events)
		}
	}
	if len(starts) != 1 || starts[0].ToolID != "toolu_1" || starts[0].ToolName != "Read" {
		t.Fatalf("starts = %+v", starts)
	}
	if got := toolArgs(events, 1); got != `{"p":1}` {
		t.Fatalf("args = %q", got)
	}
}
