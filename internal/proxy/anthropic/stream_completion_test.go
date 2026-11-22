package anthropic

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

const completionSecret = "PAYLOAD_SECRET_4A"

func completionEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func completionStream(parts ...string) string {
	return strings.Join(parts, "")
}

func completionStart() string {
	return completionEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":3,"output_tokens":0}}}`)
}

func completionText(text string) string {
	return completionStream(
		completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
}

func completionTool(frag string) string {
	return completionStream(
		completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+frag+`"}}`),
	)
}

func completionClosedTool() string {
	return completionStream(
		completionTool(`{\"city\":\"SF\"}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
}

func completionReason(reason string, output int) string {
	return completionEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+reason+`"},"usage":{"output_tokens":`+itoa(output)+`}}`)
}

func completionStop() string {
	return completionEvent("message_stop", `{"type":"message_stop"}`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func collectCompletion(t *testing.T, body string, strict bool) ([]ir.StreamEvent, error) {
	t.Helper()
	var events []ir.StreamEvent
	err := DecodeStreamWithOptions(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}, DecodeStreamOptions{StrictCompletion: strict})
	return events, err
}

func finishEvent(events []ir.StreamEvent) (ir.StreamEvent, int) {
	var fin ir.StreamEvent
	n := 0
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			fin = ev
			n++
		}
	}
	return fin, n
}

func eventText(events []ir.StreamEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventTextStart || ev.Kind == ir.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func eventArgs(events []ir.StreamEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta {
			b.WriteString(ev.ArgsFrag)
		}
	}
	return b.String()
}

func assertPayloadFree(t *testing.T, err error, events []ir.StreamEvent, secret string) {
	t.Helper()
	if secret == "" {
		return
	}
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked payload: %v", err)
	}
	for _, ev := range events {
		if strings.Contains(ev.Text, secret) || strings.Contains(ev.ArgsFrag, secret) || strings.Contains(ev.Data, secret) || strings.Contains(ev.ToolName, secret) || strings.Contains(ev.ToolID, secret) {
			t.Fatalf("event leaked payload: %+v", ev)
		}
	}
}

func assertNoFinish(t *testing.T, events []ir.StreamEvent) {
	t.Helper()
	if _, n := finishEvent(events); n != 0 {
		t.Fatalf("finish emitted: %+v", events)
	}
}

func TestDecodeStreamCompletionMatrix(t *testing.T) {
	closed := completionStream(completionStart(), completionText("hello"))
	cases := []struct {
		name       string
		body       string
		strict     bool
		wantErr    string
		wantReason ir.StopReason
		wantText   string
		wantArgs   string
		wantOut    int
		secret     string
		noEvents   bool
	}{
		{
			name: "default complete message_stop", strict: false,
			body:       completionStream(closed, completionReason("end_turn", 2), completionStop()),
			wantReason: ir.StopEndTurn, wantText: "hello", wantOut: 2,
		},
		{
			name: "strict complete message_stop", strict: true,
			body:       completionStream(closed, completionReason("end_turn", 2), completionStop()),
			wantReason: ir.StopEndTurn, wantText: "hello", wantOut: 2,
		},
		{
			name: "strict terminal eof", strict: true,
			body:       completionStream(closed, completionReason("max_tokens", 7)),
			wantReason: ir.StopMaxTokens, wantText: "hello", wantOut: 7,
		},
		{
			name: "default terminal eof still finishes", strict: false,
			body:       completionStream(closed, completionReason("max_tokens", 7)),
			wantReason: ir.StopMaxTokens, wantText: "hello", wantOut: 7,
		},
		{
			name: "strict tool terminal eof", strict: true,
			body:       completionStream(completionStart(), completionClosedTool(), completionReason("tool_use", 4)),
			wantReason: ir.StopToolUse, wantArgs: `{"city":"SF"}`, wantOut: 4,
		},
		{
			name: "strict empty message_stop", strict: true,
			body:       completionStream(completionStart(), completionStop()),
			wantReason: ir.StopEndTurn,
		},
		{
			name: "strict empty terminal eof", strict: true,
			body:       completionStream(completionStart(), completionReason("end_turn", 1)),
			wantReason: ir.StopEndTurn, wantOut: 1,
		},
		{
			name: "strict message_stop without reason", strict: true,
			body:       completionStream(completionStart(), completionEvent("message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":1}}`), completionStop()),
			wantReason: ir.StopEndTurn, wantOut: 1,
		},
		{
			name: "strict null reason is not terminal", strict: true,
			body:    completionStream(closed, completionEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":1}}`)),
			wantErr: "stream ended before completion",
		},
		{
			name: "strict empty string reason is terminal", strict: true,
			body:       completionStream(closed, completionEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":""},"usage":{"output_tokens":1}}`)),
			wantReason: ir.StopEndTurn, wantText: "hello", wantOut: 1,
		},
		{
			name: "default start only finishes", strict: false,
			body: completionStart(), wantReason: ir.StopEndTurn,
		},
		{
			name: "strict start only", strict: true,
			body: completionStart(), wantErr: "stream ended before completion",
		},
		{
			name: "strict text without terminal", strict: true,
			body: closed, wantErr: "stream ended before completion",
		},
		{
			name: "default text without terminal finishes", strict: false,
			body: closed, wantReason: ir.StopEndTurn, wantText: "hello",
		},
		{
			name: "strict open tool", strict: true,
			body:    completionStream(completionStart(), completionTool(`{\"a\":1`)),
			wantErr: "stream ended with an open content block",
		},
		{
			name: "default open tool finishes", strict: false,
			body:       completionStream(completionStart(), completionTool(`{\"a\":1`)),
			wantReason: ir.StopEndTurn, wantArgs: `{"a":1`,
		},
		{
			name: "strict open text with terminal", strict: true,
			body:    completionStream(completionStart(), completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`), completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`), completionReason("end_turn", 1)),
			wantErr: "stream ended with an open content block",
		},
		{
			name: "strict open tool with terminal stop", strict: true,
			body:    completionStream(completionStart(), completionTool(`{\"a\":1`), completionReason("tool_use", 1), completionStop()),
			wantErr: "stream ended with an open content block",
		},
		{
			name: "strict open thinking", strict: true,
			body:    completionStream(completionStart(), completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`), completionReason("end_turn", 1)),
			wantErr: "thinking block closed by stream end",
		},
		{
			name: "strict open server", strict: true,
			body:    completionStream(completionStart(), completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s","name":"w","input":{}}}`), completionReason("end_turn", 1)),
			wantErr: "server block closed by stream end",
		},
		{
			name: "strict content before start", strict: true,
			body:    completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+completionSecret+`"}}`),
			wantErr: "stream event before message_start", secret: completionSecret, noEvents: true,
		},
		{
			name: "default content before start finishes", strict: false,
			body:       completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
			wantReason: ir.StopEndTurn, wantText: "hi",
		},
		{
			name: "strict message_delta before start", strict: true,
			body:    completionReason("end_turn", 1),
			wantErr: "stream event before message_start", noEvents: true,
		},
		{
			name: "default message_delta before start is empty", strict: false,
			body: completionReason("end_turn", 1), noEvents: true,
		},
		{
			name: "strict message_stop before start", strict: true,
			body: completionStop(), wantErr: "stream event before message_start", noEvents: true,
		},
		{
			name: "default message_stop before start finishes", strict: false,
			body: completionStop(), wantReason: ir.StopEndTurn,
		},
		{
			name: "strict second start", strict: true,
			body:    completionStream(completionStart(), completionEvent("message_start", `{"type":"message_start","message":{"id":"`+completionSecret+`","model":"m"}}`)),
			wantErr: "message_start before message_stop", secret: completionSecret,
		},
		{
			name: "default second start finishes", strict: false,
			body:       completionStream(completionStart(), completionStart()),
			wantReason: ir.StopEndTurn,
		},
		{
			name: "strict malformed terminal", strict: true,
			body:    completionStream(closed, completionEvent("message_delta", `{"delta":{"stop_reason":"end_turn",`+completionSecret)),
			wantErr: "malformed stream event", secret: completionSecret,
		},
		{
			name: "strict nonstring reason", strict: true,
			body:    completionStream(closed, completionEvent("message_delta", `{"delta":{"stop_reason":1,"note":"`+completionSecret+`"}}`)),
			wantErr: "malformed stream event", secret: completionSecret,
		},
		{
			name: "strict malformed stop", strict: true,
			body:    completionStream(closed, completionReason("end_turn", 1), completionEvent("message_stop", completionSecret)),
			wantErr: "malformed stream event", secret: completionSecret,
		},
		{
			name: "default malformed stop still finishes", strict: false,
			body:       completionStream(closed, completionReason("end_turn", 1), completionEvent("message_stop", completionSecret)),
			wantReason: ir.StopEndTurn, wantText: "hello", secret: completionSecret,
		},
		{
			name: "strict start without message", strict: true,
			body:    completionEvent("message_start", `{"type":"message_start","leak":"`+completionSecret+`"}`),
			wantErr: "malformed stream event", secret: completionSecret, noEvents: true,
		},
		{
			name: "strict text delta without start", strict: true,
			body:    completionStream(completionStart(), completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)),
			wantErr: "text delta without an open text block",
		},
		{
			name: "strict tool delta without start", strict: true,
			body:    completionStream(completionStart(), completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)),
			wantErr: "input delta without an open tool block",
		},
		{
			name: "default tool delta without start relays", strict: false,
			body:       completionEvent("content_block_delta", `{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"o\":1}"}}`),
			wantReason: ir.StopEndTurn, wantArgs: `{"o":1}`,
		},
		{
			name: "strict ping after start", strict: true,
			body:    completionStream(completionStart(), completionEvent("ping", `{}`)),
			wantErr: "stream ended before completion",
		},
		{
			name: "strict empty", strict: true, body: "", noEvents: true,
		},
		{
			name: "strict comment", strict: true, body: ": ping\n\n", noEvents: true,
		},
		{
			name: "strict ping only", strict: true, body: "event: ping\ndata: {}\n\n", noEvents: true,
		},
		{
			name: "default empty", strict: false, body: "", noEvents: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, err := collectCompletion(t, tc.body, tc.strict)
			assertPayloadFree(t, err, events, tc.secret)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				sf, ok := ir.AsStreamFailure(err)
				if !ok || sf.Type != "invalid_request_error" {
					t.Fatalf("failure = %+v, want invalid_request_error", err)
				}
				assertNoFinish(t, events)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.noEvents {
				if len(events) != 0 {
					t.Fatalf("events = %+v, want none", events)
				}
				return
			}
			fin, n := finishEvent(events)
			if n != 1 {
				t.Fatalf("finish count = %d, events = %+v", n, events)
			}
			if tc.wantReason != "" && fin.StopReason != tc.wantReason {
				t.Fatalf("stop = %q, want %q", fin.StopReason, tc.wantReason)
			}
			if tc.wantText != "" && eventText(events) != tc.wantText {
				t.Fatalf("text = %q, want %q", eventText(events), tc.wantText)
			}
			if tc.wantArgs != "" && eventArgs(events) != tc.wantArgs {
				t.Fatalf("args = %q, want %q", eventArgs(events), tc.wantArgs)
			}
			if tc.wantOut != 0 && fin.OutputTokens != tc.wantOut {
				t.Fatalf("output = %d, want %d", fin.OutputTokens, tc.wantOut)
			}
		})
	}
}

func TestDecodeStreamStrictClosedBlockKinds(t *testing.T) {
	body := completionStream(
		completionStart(),
		completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"chain"}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		completionEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque"}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		completionEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":2}`),
		completionEvent("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"ty\":\"SF\"}"}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":3}`),
		completionEvent("content_block_start", `{"type":"content_block_start","index":4,"content_block":{"type":"server_tool_use","id":"srv","name":"web_search","input":{}}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":4}`),
	)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"message_stop", completionStream(body, completionReason("tool_use", 4), completionStop())},
		{"terminal eof", completionStream(body, completionReason("tool_use", 4))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, err := collectCompletion(t, tc.body, true)
			if err != nil {
				t.Fatal(err)
			}
			fin, n := finishEvent(events)
			if n != 1 || fin.StopReason != ir.StopToolUse || eventArgs(events) != `{"city":"SF"}` || eventText(events) != "answer" {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

func TestDecodeStreamStrictTerminalInvalidation(t *testing.T) {
	base := completionStream(completionStart(), completionText("hello"), completionReason("max_tokens", 2))
	events, err := collectCompletion(t, completionStream(base, completionEvent("content_block_stop", `{"type":"content_block_stop","index":9}`)), true)
	if err == nil || !strings.Contains(err.Error(), "stream ended before completion") {
		t.Fatalf("content after terminal = %v, events = %+v", err, events)
	}
	assertNoFinish(t, events)
	events, err = collectCompletion(t, completionStream(base, completionEvent("content_block_stop", `{"type":"content_block_stop","index":9}`), completionReason("end_turn", 3)), true)
	if err != nil {
		t.Fatal(err)
	}
	fin, n := finishEvent(events)
	if n != 1 || fin.StopReason != ir.StopEndTurn || fin.OutputTokens != 3 {
		t.Fatalf("restored terminal = %+v", fin)
	}
	events, err = collectCompletion(t, completionStream(base, completionStop()), true)
	if err != nil {
		t.Fatal(err)
	}
	fin, n = finishEvent(events)
	if n != 1 || fin.StopReason != ir.StopMaxTokens {
		t.Fatalf("message_stop reason = %+v", fin)
	}
}

func TestDecodeStreamStrictToolBackfillAndReuse(t *testing.T) {
	backfill := completionStream(
		completionStart(),
		completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1"}}`),
		completionEvent("content_block_start", `{"type":"content_block_start","index":5,"content_block":{"type":"text","text":""}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":5}`),
		completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		completionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":",\"b\":2}"}}`),
		completionEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		completionReason("tool_use", 4),
	)
	events, err := collectCompletion(t, backfill, true)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart && ev.Index == 0 {
			starts++
			if ev.ToolID != "toolu_1" || ev.ToolName != "get_weather" {
				t.Fatalf("backfill = %+v", ev)
			}
		}
	}
	if starts != 2 || eventArgs(events) != `{"a":1,"b":2}` {
		t.Fatalf("starts = %d args = %q", starts, eventArgs(events))
	}
	reused := completionStream(
		completionStart(),
		completionClosedTool(),
		completionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_2","name":"`+completionSecret+`","input":{}}}`),
	)
	events, err = collectCompletion(t, reused, true)
	if err == nil || !strings.Contains(err.Error(), "tool block restarted after stop") {
		t.Fatalf("reuse = %v", err)
	}
	assertPayloadFree(t, err, events, completionSecret)
	assertNoFinish(t, events)
}

func TestDecodeStreamStrictContentIndexes(t *testing.T) {
	indexes := []struct {
		name, raw string
	}{
		{"missing", ""},
		{"null", "null"},
		{"negative", "-1"},
		{"fraction", "0.5"},
		{"decimal", "0.0"},
		{"exponent", "0e0"},
		{"string", `"0"`},
		{"boolean", "false"},
		{"array", "[]"},
		{"object", "{}"},
		{"overflow", "18446744073709551616"},
	}
	for _, kind := range []string{"start", "delta", "stop"} {
		for _, index := range indexes {
			t.Run(kind+"/"+index.name, func(t *testing.T) {
				data := `{"note":"` + completionSecret + `"`
				if index.raw != "" {
					data += `,"index":` + index.raw
				}
				prefix, suffix := completionStart(), ""
				switch kind {
				case "start":
					data += `,"content_block":{"type":"text","text":"` + completionSecret + `"}`
					suffix = completionEvent("content_block_stop", `{"index":0}`)
				case "delta":
					prefix += completionEvent("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`)
					data += `,"delta":{"type":"text_delta","text":"` + completionSecret + `"}`
					suffix = completionEvent("content_block_stop", `{"index":0}`)
				case "stop":
					prefix += completionTool(`{\"city\":\"SF\"}`)
				}
				data += `}`
				body := completionStream(prefix, completionEvent("content_block_"+kind, data), suffix, completionReason("end_turn", 2), completionStop())
				events, err := collectCompletion(t, body, true)
				sf, ok := ir.AsStreamFailure(err)
				if !ok || sf.Type != "invalid_request_error" || sf.Message != "malformed content block index" {
					t.Fatalf("error = %v, want malformed content block index", err)
				}
				assertNoFinish(t, events)
				assertPayloadFree(t, err, events, completionSecret)
				if kind == "stop" && eventArgs(events) != `{"city":"SF"}` {
					t.Fatalf("tool args = %q", eventArgs(events))
				}
			})
		}
	}
	for _, index := range []string{"0", "7", strconv.Itoa(int(^uint(0) >> 1))} {
		t.Run("valid/"+index, func(t *testing.T) {
			body := completionStream(
				completionStart(),
				completionEvent("content_block_start", `{"index":`+index+`,"content_block":{"type":"text","text":""}}`),
				completionEvent("content_block_delta", `{"index":`+index+`,"delta":{"type":"text_delta","text":"ok"}}`),
				completionEvent("content_block_stop", `{"index":`+index+`}`),
				completionReason("end_turn", 2),
			)
			events, err := collectCompletion(t, body, true)
			if _, n := finishEvent(events); err != nil || n != 1 || eventText(events) != "ok" {
				t.Fatalf("valid index: err=%v events=%+v", err, events)
			}
		})
	}
	for _, index := range []string{"", `,"index":null`} {
		t.Run("default/"+index, func(t *testing.T) {
			body := completionStream(
				completionStart(),
				completionEvent("content_block_start", `{"content_block":{"type":"text","text":""}`+index+`}`),
				completionEvent("content_block_delta", `{"delta":{"type":"text_delta","text":"legacy"}`+index+`}`),
				completionEvent("content_block_stop", `{`+strings.TrimPrefix(index, ",")+`}`),
				completionReason("end_turn", 2), completionStop(),
			)
			events, err := collectCompletion(t, body, false)
			if _, n := finishEvent(events); err != nil || n != 1 || eventText(events) != "legacy" {
				t.Fatalf("default index behavior: err=%v events=%+v", err, events)
			}
		})
	}
}

func completionRedacted(index int, data string) string {
	return completionEvent("content_block_start", `{"index":`+strconv.Itoa(index)+`,"content_block":{"type":"redacted_thinking","data":"`+data+`"}}`)
}

func TestDecodeStreamStrictRedactedLifecycle(t *testing.T) {
	start := completionRedacted(0, "opaque")
	stop := completionEvent("content_block_stop", `{"index":0}`)
	for _, ending := range []struct {
		name, body string
	}{
		{"eof", ""},
		{"terminal-eof", completionReason("end_turn", 2)},
		{"message-stop", completionStop()},
	} {
		for _, strict := range []bool{false, true} {
			t.Run(ending.name+"/strict="+strconv.FormatBool(strict), func(t *testing.T) {
				events, err := collectCompletion(t, completionStream(completionStart(), start, ending.body), strict)
				if strict {
					sf, ok := ir.AsStreamFailure(err)
					if !ok || sf.Type != "invalid_request_error" || sf.Message != "stream ended with an open content block" {
						t.Fatalf("open redacted block: %v", err)
					}
					assertNoFinish(t, events)
				} else if _, n := finishEvent(events); err != nil || n != 1 {
					t.Fatalf("default redacted behavior: err=%v events=%+v", err, events)
				}
			})
		}
	}
	for _, ending := range []string{completionReason("max_tokens", 2), completionReason("max_tokens", 2) + completionStop()} {
		events, err := collectCompletion(t, completionStream(completionStart(), start, stop, ending), true)
		fin, n := finishEvent(events)
		if err != nil || n != 1 || fin.StopReason != ir.StopMaxTokens {
			t.Fatalf("closed redacted block: err=%v events=%+v", err, events)
		}
		redacted := 0
		for _, ev := range events {
			if ev.Kind == ir.EventReasoningEnd {
				t.Fatalf("redacted source stop emitted another IR close: %+v", ev)
			}
			if ev.Kind == ir.EventRedactedReasoning {
				redacted++
				if ev.Index != 0 || ev.Data != "opaque" {
					t.Fatalf("redacted data changed: %+v", ev)
				}
			}
		}
		if redacted != 1 {
			t.Fatalf("redacted count = %d", redacted)
		}
	}
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"duplicate", start + completionRedacted(0, completionSecret), "content block started on an open redacted thinking block"},
		{"spent-redacted", start + stop + completionRedacted(0, completionSecret), "redacted thinking block restarted after stop"},
		{"spent-text", completionText("ok") + completionRedacted(0, completionSecret), "redacted thinking block restarted after stop"},
		{"wrong-stop", start + completionEvent("content_block_stop", `{"index":7}`) + completionReason("end_turn", 2), "stream ended with an open content block"},
		{"open-text", completionEvent("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`) + completionRedacted(0, completionSecret), "redacted thinking block started on an open content block"},
		{"open-tool", completionTool(`{}`) + completionRedacted(0, completionSecret), "redacted thinking block started on an open content block"},
		{"open-server", completionEvent("content_block_start", `{"index":0,"content_block":{"type":"server_tool_use","id":"s","name":"w","input":{}}}`) + completionRedacted(0, completionSecret), "redacted thinking block started on an open content block"},
		{"delta", start + completionEvent("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"`+completionSecret+`"}}`), "delta on a redacted thinking block"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, err := collectCompletion(t, completionStart()+tc.body, true)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			assertNoFinish(t, events)
			assertPayloadFree(t, err, events, completionSecret)
		})
	}
	for _, kind := range []string{"text", "tool_use", "thinking", "server_tool_use"} {
		for _, closed := range []bool{false, true} {
			t.Run("reuse/"+kind+"/closed="+strconv.FormatBool(closed), func(t *testing.T) {
				prefix := completionStart() + start
				if closed {
					prefix += stop
				}
				body := prefix + completionEvent("content_block_start", `{"index":0,"content_block":{"type":"`+kind+`","text":"`+completionSecret+`","id":"s","name":"w","input":{}}}`)
				events, err := collectCompletion(t, body, true)
				if err == nil {
					t.Fatal("redacted index reuse accepted")
				}
				assertNoFinish(t, events)
				assertPayloadFree(t, err, events, completionSecret)
			})
		}
	}
}

func TestDecodeStreamStrictRedactedBoundsAndBackfill(t *testing.T) {
	var opens, closes strings.Builder
	for i := 0; i < maxOpenThinkingBlocks; i++ {
		opens.WriteString(completionRedacted(i, "opaque"))
		closes.WriteString(completionEvent("content_block_stop", `{"index":`+strconv.Itoa(i)+`}`))
	}
	events, err := collectCompletion(t, completionStart()+opens.String()+closes.String()+completionReason("end_turn", 2), true)
	if _, n := finishEvent(events); err != nil || n != 1 {
		t.Fatalf("at capacity: err=%v events=%+v", err, events)
	}
	events, err = collectCompletion(t, completionStart()+opens.String()+completionRedacted(maxOpenThinkingBlocks, completionSecret), true)
	if err == nil || !strings.Contains(err.Error(), "too many open redacted thinking blocks") {
		t.Fatalf("over capacity: %v", err)
	}
	assertNoFinish(t, events)
	assertPayloadFree(t, err, events, completionSecret)
	var sequential strings.Builder
	sequential.WriteString(completionStart())
	for i := 0; i < 3*maxOpenThinkingBlocks; i++ {
		sequential.WriteString(completionRedacted(i, "opaque"))
		sequential.WriteString(completionEvent("content_block_stop", `{"index":`+strconv.Itoa(i)+`}`))
	}
	sequential.WriteString(completionReason("end_turn", 2))
	events, err = collectCompletion(t, sequential.String(), true)
	if _, n := finishEvent(events); err != nil || n != 1 {
		t.Fatalf("closed indexes retained: err=%v events=%+v", err, events)
	}
	body := completionStream(
		completionStart(), completionTool(`{\"a\":1`),
		completionRedacted(5, "opaque"), completionEvent("content_block_stop", `{"index":5}`),
		completionEvent("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		completionEvent("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":",\"b\":2}"}}`),
		completionEvent("content_block_stop", `{"index":0}`), completionReason("tool_use", 4),
	)
	events, err = collectCompletion(t, body, true)
	fin, n := finishEvent(events)
	if err != nil || n != 1 || fin.StopReason != ir.StopToolUse || eventArgs(events) != `{"a":1,"b":2}` {
		t.Fatalf("tool backfill after higher redacted stop: err=%v events=%+v", err, events)
	}
}

func TestDecodeStreamStrictStopsAtMessageStop(t *testing.T) {
	body := completionStream(completionStart(), completionText("hello"), completionReason("end_turn", 1), completionStop(), completionEvent("error", `{"type":"error","error":{"type":"api_error","message":"`+completionSecret+`"}}`))
	events, err := collectCompletion(t, body, true)
	if err != nil {
		t.Fatal(err)
	}
	assertPayloadFree(t, err, events, completionSecret)
	if _, n := finishEvent(events); n != 1 || eventText(events) != "hello" {
		t.Fatalf("events = %+v", events)
	}
	events, err = collectCompletion(t, body, false)
	if err == nil || !strings.Contains(err.Error(), completionSecret) {
		t.Fatalf("default after stop = %v", err)
	}
	if _, n := finishEvent(events); n != 1 {
		t.Fatalf("default finish count = %d", n)
	}
}

func TestDecodeStreamCompletionFailuresDoNotFinish(t *testing.T) {
	prefix := completionStream(completionStart(), completionText("hello"), completionReason("end_turn", 1))
	events, err := collectCompletionReader(t, &suffixErrorReader{r: strings.NewReader(prefix), err: errors.New("upstream read failed")}, true)
	if err == nil || err.Error() != "upstream read failed" {
		t.Fatalf("read error = %v", err)
	}
	assertNoFinish(t, events)
	errorBody := completionStream(prefix, completionEvent("error", `{"type":"error","error":{"type":"api_error","message":"provider failed"}}`))
	events, err = collectCompletion(t, errorBody, true)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Type != "api_error" || sf.Message != "provider failed" {
		t.Fatalf("provider error = %v", err)
	}
	assertNoFinish(t, events)
	events, err = collectCompletionReader(t, &suffixErrorReader{r: strings.NewReader(completionStart()), err: context.Canceled}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	assertNoFinish(t, events)
}

func collectCompletionReader(t *testing.T, r io.Reader, strict bool) ([]ir.StreamEvent, error) {
	t.Helper()
	var events []ir.StreamEvent
	err := DecodeStreamWithOptions(r, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}, DecodeStreamOptions{StrictCompletion: strict})
	return events, err
}

type suffixErrorReader struct {
	r    io.Reader
	err  error
	done bool
}

func (s *suffixErrorReader) Read(p []byte) (int, error) {
	if s.done {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	if err == io.EOF {
		s.done = true
		if n > 0 {
			return n, nil
		}
		return 0, s.err
	}
	return n, err
}
