package responses

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// Exact four-event HAR sequence from the Codex overload capture (lifecycle only;
// response.failed carries the error object without the huge instructions blob).
const harOverloadSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_overload","model":"up","status":"in_progress"}}

event: response.in_progress
data: {"type":"response.in_progress","response":{"id":"resp_overload","model":"up","status":"in_progress"}}

event: error
data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null},"sequence_number":2}

event: response.failed
data: {"type":"response.failed","response":{"id":"resp_overload","object":"response","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"output":[],"usage":null}}

`

func collectStream(t *testing.T, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	return out, err
}

func TestDecodeStreamHAROverload(t *testing.T) {
	evs, err := collectStream(t, harOverloadSSE)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want *ir.StreamFailure, got %v", err)
	}
	if sf.Code != "server_is_overloaded" {
		t.Errorf("code = %q", sf.Code)
	}
	if sf.Type != "service_unavailable_error" {
		t.Errorf("type = %q", sf.Type)
	}
	if !strings.Contains(sf.Message, "overloaded") {
		t.Errorf("message = %q", sf.Message)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventFinish {
			t.Fatalf("must not emit EventFinish on failure: %+v", evs)
		}
	}
	if len(evs) == 0 || evs[0].Kind != ir.EventMessageStart {
		t.Fatalf("want MessageStart from created, got %+v", evs)
	}
}

func TestDecodeStreamFailedWithoutPrecedingError(t *testing.T) {
	body := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.failed
data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_is_overloaded","message":"overloaded"}}}

`
	evs, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "server_is_overloaded" {
		t.Errorf("code = %q", sf.Code)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventFinish {
			t.Fatal("unexpected Finish")
		}
	}
}

func TestDecodeStreamTopLevelErrorBeforeCreated(t *testing.T) {
	body := `event: error
data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"overloaded now"}}

`
	evs, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "server_is_overloaded" {
		t.Errorf("code = %q", sf.Code)
	}
	if len(evs) != 0 {
		t.Fatalf("want no events before created, got %+v", evs)
	}
}

func TestDecodeStreamCompletedUsageIntact(t *testing.T) {
	body := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hi"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}

`
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var finish *ir.StreamEvent
	for i := range evs {
		switch evs[i].Kind {
		case ir.EventTextDelta:
			text += evs[i].Text
		case ir.EventFinish:
			finish = &evs[i]
		}
	}
	if text != "hi" {
		t.Errorf("text = %q", text)
	}
	if finish == nil || finish.InputTokens != 7 || finish.OutputTokens != 3 {
		t.Fatalf("finish = %+v", finish)
	}
	if finish.StopReason != ir.StopEndTurn {
		t.Errorf("stop = %q", finish.StopReason)
	}
}

func TestDecodeStreamIncompleteMaxTokens(t *testing.T) {
	body := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"x"}

event: response.incomplete
data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","usage":{"input_tokens":1,"output_tokens":9}}}

`
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var finish *ir.StreamEvent
	for i := range evs {
		if evs[i].Kind == ir.EventFinish {
			finish = &evs[i]
		}
	}
	if finish == nil || finish.StopReason != ir.StopMaxTokens {
		t.Fatalf("finish = %+v, want max tokens", finish)
	}
}

func TestDecodeResponseRejectsFailed(t *testing.T) {
	body := `{"id":"r1","model":"m","status":"failed","error":{"code":"server_is_overloaded","message":"overloaded"},"output":[],"usage":null}`
	_, err := DecodeResponse([]byte(body))
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "server_is_overloaded" {
		t.Errorf("code = %q", sf.Code)
	}
}

func TestDecodeStreamTopLevelErrorNumericCode(t *testing.T) {
	body := `event: error
data: {"type":"error","error":{"type":"service_unavailable_error","code":429,"message":"overloaded now"}}

`
	evs, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "429" {
		t.Errorf("code = %q", sf.Code)
	}
	if sf.Type != "service_unavailable_error" {
		t.Errorf("type = %q", sf.Type)
	}
	if len(evs) != 0 {
		t.Fatalf("want no events before created, got %+v", evs)
	}
}

func TestDecodeStreamResponseFailedNumericCode(t *testing.T) {
	body := `event: response.failed
data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":429,"message":"overloaded"}}}

`
	evs, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "429" {
		t.Errorf("code = %q", sf.Code)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventFinish {
			t.Fatal("unexpected Finish")
		}
	}
}

func TestDecodeStreamCreatedThenFailedNonStringMetadata(t *testing.T) {
	tests := []struct {
		name     string
		failed   string
		wantCode string
	}{
		{
			name:     "numeric code",
			failed:   `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":429,"message":"overloaded"}}}`,
			wantCode: "429",
		},
		{
			name:     "unsupported code",
			failed:   `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":true,"type":"server_error","message":"overloaded"}}}`,
			wantCode: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.failed
data: ` + tc.failed + "\n\n"
			evs, err := collectStream(t, body)
			sf, ok := ir.AsStreamFailure(err)
			if !ok {
				t.Fatalf("want StreamFailure, got %v", err)
			}
			if sf.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", sf.Code, tc.wantCode)
			}
			hasStart := false
			for _, ev := range evs {
				if ev.Kind == ir.EventFinish {
					t.Fatalf("must not emit EventFinish: %+v", evs)
				}
				if ev.Kind == ir.EventMessageStart {
					hasStart = true
				}
			}
			if !hasStart {
				t.Fatalf("want MessageStart from created, got %+v", evs)
			}
		})
	}
}

func TestDecodeStreamErrorThenFailedMergesNumericNullCode(t *testing.T) {
	tests := []struct {
		name     string
		error    string
		failed   string
		wantCode string
		wantType string
	}{
		{
			name:     "numeric prior code fills failed gap",
			error:    `{"type":"error","error":{"type":"service_unavailable_error","code":429,"message":"overloaded now"}}`,
			failed:   `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"message":"overloaded later"}}}`,
			wantCode: "429",
			wantType: "service_unavailable_error",
		},
		{
			name:     "null failed code keeps prior numeric",
			error:    `{"type":"error","error":{"type":"service_unavailable_error","code":429,"message":"overloaded now"}}`,
			failed:   `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":null,"message":"overloaded later"}}}`,
			wantCode: "429",
			wantType: "service_unavailable_error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := "event: error\ndata: " + tc.error + "\n\nevent: response.failed\ndata: " + tc.failed + "\n\n"
			_, err := collectStream(t, body)
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
			if sf.Message != "overloaded later" {
				t.Errorf("message = %q", sf.Message)
			}
		})
	}
}

func TestDecodeStreamFailedUnusableMetadata(t *testing.T) {
	body := `event: response.failed
data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":true,"type":["x"],"message":{"n":1}}}}

`
	_, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Message != "upstream response failed" {
		t.Errorf("message = %q", sf.Message)
	}
	if sf.Code != "" || sf.Type != "" {
		t.Errorf("want empty metadata, got %+v", sf)
	}
}

func TestDecodeStreamFailedStatusOnNonFailedType(t *testing.T) {
	body := `event: response.completed
data: {"type":"response.completed","response":{"id":"r1","status":"failed","error":{"code":429,"message":"nope","type":"api_error"}}}

`
	evs, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "429" {
		t.Errorf("code = %q", sf.Code)
	}
	if sf.Type != "api_error" {
		t.Errorf("type = %q", sf.Type)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventFinish {
			t.Fatalf("must not emit EventFinish: %+v", evs)
		}
	}
}

type decodedCall struct {
	index  int
	id     string
	name   string
	args   string
	deltas int
}

func assembleCalls(evs []ir.StreamEvent) ([]decodedCall, *ir.StreamEvent) {
	byIndex := map[int]int{}
	var calls []decodedCall
	var finish *ir.StreamEvent
	for i := range evs {
		ev := evs[i]
		switch ev.Kind {
		case ir.EventToolCallStart:
			n, ok := byIndex[ev.Index]
			if !ok {
				n = len(calls)
				byIndex[ev.Index] = n
				calls = append(calls, decodedCall{index: ev.Index})
			}
			if ev.ToolID != "" {
				calls[n].id = ev.ToolID
			}
			if ev.ToolName != "" {
				calls[n].name = ev.ToolName
			}
		case ir.EventToolCallDelta:
			n, ok := byIndex[ev.Index]
			if !ok {
				n = len(calls)
				byIndex[ev.Index] = n
				calls = append(calls, decodedCall{index: ev.Index})
			}
			if ev.ArgsFrag != "" {
				calls[n].args += ev.ArgsFrag
				calls[n].deltas++
			}
		case ir.EventFinish:
			finish = &evs[i]
		}
	}
	return calls, finish
}

func sseData(events ...string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: ")
		b.WriteString(ev)
		b.WriteString("\n\n")
	}
	return b.String()
}

func fnItem(id, callID, name, args string) string {
	return fmt.Sprintf(`{"id":%q,"type":"function_call","status":"in_progress","call_id":%q,"name":%q,"arguments":%q}`, id, callID, name, args)
}

func readArgs(path string) string {
	return fmt.Sprintf(`{"limit":25,"offset":1,"path":%q}`, path)
}

func addedEvent(index int, id, callID, name, args string) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":%s}`, index, fnItem(id, callID, name, args))
}

func argsDoneEvent(index int, itemID, name, args string) string {
	return fmt.Sprintf(`{"type":"response.function_call_arguments.done","item_id":%q,"output_index":%d,"name":%q,"arguments":%q}`, itemID, index, name, args)
}

func itemDoneEvent(index int, id, callID, name, args string) string {
	item := fmt.Sprintf(`{"id":%q,"type":"function_call","status":"completed","call_id":%q,"name":%q,"arguments":%q}`, id, callID, name, args)
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item)
}

func completedEvent(output, usage string) string {
	if usage == "" {
		usage = `{"input_tokens":11,"output_tokens":4}`
	}
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_test","model":"gpt-test","status":"completed","output":%s,"usage":%s}}`, output, usage)
}

func finalOutput(items ...string) string {
	return "[" + strings.Join(items, ",") + "]"
}

func completedItem(id, callID, name, args string) string {
	return fmt.Sprintf(`{"id":%q,"type":"function_call","status":"completed","call_id":%q,"name":%q,"arguments":%q}`, id, callID, name, args)
}

func assertCalls(t *testing.T, calls []decodedCall, want []decodedCall) {
	t.Helper()
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %d", calls, len(want))
	}
	for i := range want {
		got := calls[i]
		if got.index != want[i].index || got.id != want[i].id || got.name != want[i].name || got.args != want[i].args {
			t.Fatalf("call %d = %+v, want %+v", i, got, want[i])
		}
		if want[i].deltas != 0 && got.deltas != want[i].deltas {
			t.Fatalf("call %d deltas = %d, want %d", i, got.deltas, want[i].deltas)
		}
	}
}

func TestDecodeStreamSnapshotOnlyPreservesArguments(t *testing.T) {
	args := readArgs("/example/BallOverlay.kt")
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_test","model":"gpt-test","status":"in_progress"}}`,
		addedEvent(2, "fc_a", "call_a", "read", ""),
		argsDoneEvent(2, "fc_a", "read", args),
		itemDoneEvent(2, "fc_a", "call_a", "read", args),
		completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, finish := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{
		index: 0, id: "call_a", name: "read", args: args, deltas: 1,
	}})
	if finish == nil || finish.StopReason != ir.StopToolUse || finish.InputTokens != 11 || finish.OutputTokens != 4 {
		t.Fatalf("finish = %+v", finish)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventToolCallStart && ev.ArgsFrag != "" {
			t.Fatalf("arguments leaked onto Start: %+v", ev)
		}
	}
}

func TestDecodeStreamRepeatedSameNameCalls(t *testing.T) {
	for n := 1; n <= 4; n++ {
		t.Run(fmt.Sprintf("%d", n), func(t *testing.T) {
			var events []string
			var want []decodedCall
			var finals []string
			for i := 0; i < n; i++ {
				path := fmt.Sprintf("/example/file-%d.kt", i)
				args := readArgs(path)
				id := fmt.Sprintf("fc_%d", i)
				callID := fmt.Sprintf("call_%d", i)
				index := i + 2
				events = append(events,
					addedEvent(index, id, callID, "read", ""),
					argsDoneEvent(index, id, "read", args),
					itemDoneEvent(index, id, callID, "read", args),
				)
				finals = append(finals, completedItem(id, callID, "read", args))
				want = append(want, decodedCall{index: i, id: callID, name: "read", args: args})
			}
			events = append(events, completedEvent(finalOutput(finals...), ""))
			evs, err := collectStream(t, sseData(events...))
			if err != nil {
				t.Fatal(err)
			}
			calls, finish := assembleCalls(evs)
			assertCalls(t, calls, want)
			if finish == nil || finish.StopReason != ir.StopToolUse {
				t.Fatalf("finish = %+v", finish)
			}
		})
	}
}

func TestDecodeStreamInterleavedFragmentSnapshots(t *testing.T) {
	a := readArgs("/example/a.kt")
	b := readArgs("/example/b.kt")
	body := sseData(
		addedEvent(1, "fc_a", "call_a", "read", ""),
		addedEvent(2, "fc_b", "call_b", "read", ""),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_b","output_index":2,"delta":%q}`, b[:8]),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":1,"delta":%q}`, a[:6]),
		argsDoneEvent(1, "fc_a", "read", a),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_b","output_index":2,"delta":%q}`, b[8:]),
		itemDoneEvent(2, "fc_b", "call_b", "read", b),
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{
		{index: 0, id: "call_a", name: "read", args: a},
		{index: 1, id: "call_b", name: "read", args: b},
	})
}

func TestDecodeStreamDeltaOnlyAndDoneOnly(t *testing.T) {
	args := readArgs("/example/only.kt")
	t.Run("delta only", func(t *testing.T) {
		body := sseData(
			addedEvent(0, "fc_a", "call_a", "read", ""),
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":%q}`, args),
			completedEvent(`[]`, ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 1}})
		if finish == nil || finish.StopReason != ir.StopToolUse {
			t.Fatalf("finish = %+v", finish)
		}
	})
	t.Run("arguments done without added", func(t *testing.T) {
		body := sseData(
			argsDoneEvent(4, "fc_late", "read", args),
			itemDoneEvent(4, "fc_late", "call_late", "read", args),
			completedEvent(`[]`, ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_late", name: "read", args: args, deltas: 1}})
	})
	t.Run("item done only", func(t *testing.T) {
		body := sseData(
			itemDoneEvent(3, "fc_item", "call_item", "read", args),
			completedEvent(`[]`, ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_item", name: "read", args: args}})
	})
}

func TestDecodeStreamFullSnapshotsEmitOnce(t *testing.T) {
	args := readArgs("/example/full.kt")
	body := sseData(
		addedEvent(1, "fc_a", "call_a", "read", ""),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":1,"delta":%q}`, args[:10]),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":1,"delta":%q}`, args[10:]),
		argsDoneEvent(1, "fc_a", "read", args),
		itemDoneEvent(1, "fc_a", "call_a", "read", args),
		completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 2}})
}

func TestDecodeStreamPartialDeltaThenDoneSuffix(t *testing.T) {
	args := readArgs("/example/partial.kt")
	body := sseData(
		addedEvent(0, "fc_a", "call_a", "read", ""),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, args[:17]),
		argsDoneEvent(0, "fc_a", "read", args),
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 2}})
	if !strings.HasPrefix(calls[0].args, args[:17]) {
		t.Fatalf("prefix lost: %q", calls[0].args)
	}
}

func TestDecodeStreamDuplicateSnapshotsDoNotDuplicate(t *testing.T) {
	args := readArgs("/example/dup.kt")
	body := sseData(
		argsDoneEvent(0, "fc_a", "read", args),
		argsDoneEvent(0, "fc_a", "read", args),
		itemDoneEvent(0, "fc_a", "call_a", "read", args),
		completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 1}})
}

func TestDecodeStreamAddedFullArgumentsDoesNotClose(t *testing.T) {
	args := readArgs("/example/added.kt")
	suffix := args[len(args)-2:]
	prefix := args[:len(args)-2]
	body := sseData(
		addedEvent(0, "fc_a", "call_a", "read", prefix),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":%q}`, suffix),
		itemDoneEvent(0, "fc_a", "call_a", "read", args),
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args}})
	start := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventToolCallStart {
			start++
		}
	}
	if start != 1 {
		t.Fatalf("starts = %d, want 1", start)
	}
}

func TestDecodeStreamIdentityConflicts(t *testing.T) {
	args := readArgs("/example/id.kt")
	tests := []struct {
		name string
		body string
	}{
		{
			name: "index",
			body: sseData(addedEvent(1, "fc_a", "call_a", "read", ""), addedEvent(2, "fc_a", "call_a", "read", args)),
		},
		{
			name: "item id",
			body: sseData(addedEvent(1, "fc_a", "call_a", "read", ""), addedEvent(1, "fc_b", "call_a", "read", args)),
		},
		{
			name: "call id",
			body: sseData(addedEvent(1, "fc_a", "call_a", "read", ""), itemDoneEvent(1, "fc_a", "call_b", "read", args)),
		},
		{
			name: "name",
			body: sseData(addedEvent(1, "fc_a", "call_a", "read", ""), itemDoneEvent(1, "fc_a", "call_a", "bash", args)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStream(t, tc.body)
			sf, ok := ir.AsStreamFailure(err)
			if !ok || !strings.Contains(sf.Message, "conflicting function call") {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), args) || strings.Contains(err.Error(), "/example") {
				t.Fatalf("error leaked payload: %v", err)
			}
		})
	}
}

func TestDecodeStreamArgumentConflicts(t *testing.T) {
	args := readArgs("/example/conflict.kt")
	other := readArgs("/example/other.kt")
	tests := []string{
		sseData(addedEvent(0, "fc_a", "call_a", "read", args), argsDoneEvent(0, "fc_a", "read", other)),
		sseData(
			addedEvent(0, "fc_a", "call_a", "read", ""),
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, args),
			argsDoneEvent(0, "fc_a", "read", args[:len(args)-1]),
		),
		sseData(
			argsDoneEvent(0, "fc_a", "read", args),
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", other)), ""),
		),
	}
	for i, body := range tests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			_, err := collectStream(t, body)
			sf, ok := ir.AsStreamFailure(err)
			if !ok || sf.Message != "conflicting function call arguments" {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "/example") || strings.Contains(err.Error(), "path") {
				t.Fatalf("error leaked arguments: %v", err)
			}
		})
	}
}

func TestDecodeStreamDeltaFirstStableIndex(t *testing.T) {
	args := readArgs("/example/delta-first.kt")
	body := sseData(
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":7,"delta":%q}`, args[:9]),
		addedEvent(7, "fc_a", "call_a", "read", ""),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":7,"delta":%q}`, args[9:]),
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args}})
}

func TestDecodeStreamArgumentsDoneFillsCallIDLater(t *testing.T) {
	args := readArgs("/example/late-id.kt")
	body := sseData(
		argsDoneEvent(5, "fc_late", "read", args),
		itemDoneEvent(5, "fc_late", "call_late", "read", args),
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var sawStart, sawDelta bool
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventToolCallStart:
			if sawDelta {
				t.Fatal("delta preceded the completed identity")
			}
			if ev.ToolID != "call_late" || ev.ToolName != "read" {
				t.Fatalf("start = %+v", ev)
			}
			sawStart = true
		case ir.EventToolCallDelta:
			sawDelta = true
		}
	}
	if !sawStart || !sawDelta {
		t.Fatalf("events = %+v", evs)
	}
}

func TestDecodeStreamFinalOnlyAndMissingCompletionOutput(t *testing.T) {
	args := readArgs("/example/final.kt")
	t.Run("final only", func(t *testing.T) {
		body := sseData(completedEvent(finalOutput(completedItem("fc_final", "call_final", "read", args)), ""))
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_final", name: "read", args: args}})
		if finish == nil || finish.StopReason != ir.StopToolUse {
			t.Fatalf("finish = %+v", finish)
		}
	})
	t.Run("missing output", func(t *testing.T) {
		body := sseData(
			addedEvent(0, "fc_a", "call_a", "read", ""),
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, args),
			completedEvent(`[]`, ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args}})
	})
	t.Run("completion without tool output", func(t *testing.T) {
		body := sseData(
			`{"type":"response.output_text.delta","delta":"ok"}`,
			completedEvent(`[]`, ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		if len(calls) != 0 || finish == nil || finish.StopReason != ir.StopEndTurn {
			t.Fatalf("calls=%+v finish=%+v", calls, finish)
		}
	})
}

func TestDecodeStreamSnapshotOnlyIncompleteStop(t *testing.T) {
	args := readArgs("/example/incomplete.kt")
	body := sseData(
		addedEvent(0, "fc_a", "call_a", "read", ""),
		argsDoneEvent(0, "fc_a", "read", args),
		`{"type":"response.incomplete","response":{"id":"resp_test","status":"incomplete","output":[],"usage":{"input_tokens":2,"output_tokens":9}}}`,
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, finish := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args}})
	if finish == nil || finish.StopReason != ir.StopMaxTokens || finish.OutputTokens != 9 {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestDecodeStreamEmptyArgumentsStayEmpty(t *testing.T) {
	body := sseData(
		addedEvent(0, "fc_empty", "call_empty", "read", ""),
		argsDoneEvent(0, "fc_empty", "read", ""),
		itemDoneEvent(0, "fc_empty", "call_empty", "read", ""),
		completedEvent(finalOutput(completedItem("fc_empty", "call_empty", "read", "")), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_empty", name: "read", args: "", deltas: 0}})
}

func TestDecodeStreamRawArgumentBytesPreserved(t *testing.T) {
	raw := " {\"path\":\"/example/raw.kt\",} "
	body := sseData(
		addedEvent(0, "fc_raw", "call_raw", "read", ""),
		argsDoneEvent(0, "fc_raw", "read", raw),
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_raw", name: "read", args: raw}})
}

func TestDecodeStreamFailurePrecedesSnapshotFallback(t *testing.T) {
	args := readArgs("/example/failed.kt")
	body := sseData(
		addedEvent(0, "fc_a", "call_a", "read", ""),
		`{"type":"response.failed","response":{"id":"resp_test","status":"failed","error":{"code":"server_is_overloaded","message":"overloaded"},"output":[`+completedItem("fc_a", "call_a", "read", args)+`]}}`,
	)
	evs, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Code != "server_is_overloaded" {
		t.Fatalf("err = %v", err)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventToolCallDelta || ev.Kind == ir.EventFinish {
			t.Fatalf("failure emitted tool or finish: %+v", evs)
		}
	}
}

func TestDecodeStreamEmitErrorPreserved(t *testing.T) {
	want := errors.New("emit stopped")
	err := DecodeStream(strings.NewReader(sseData(addedEvent(0, "fc_a", "call_a", "read", ""))), func(ir.StreamEvent) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeStreamToolCaps(t *testing.T) {
	t.Run("calls", func(t *testing.T) {
		var events []string
		for i := 0; i < 3; i++ {
			events = append(events, addedEvent(i, fmt.Sprintf("fc_%d", i), fmt.Sprintf("call_%d", i), "read", ""))
		}
		err := decodeStreamLimited(strings.NewReader(sseData(events...)), func(ir.StreamEvent) error { return nil }, 32, 2)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "too many function calls" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		err := decodeStreamLimited(strings.NewReader(sseData(
			addedEvent(0, "fc_a", "call_a", "read", ""),
			argsDoneEvent(0, "fc_a", "read", "abcdef"),
		)), func(ir.StreamEvent) error { return nil }, 5, 4)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "function call arguments too large" {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "abcdef") {
			t.Fatalf("error leaked arguments: %v", err)
		}
	})
	t.Run("duplicate snapshot does not consume cap", func(t *testing.T) {
		err := decodeStreamLimited(strings.NewReader(sseData(
			argsDoneEvent(0, "fc_a", "read", "abcd"),
			argsDoneEvent(0, "fc_a", "read", "abcd"),
		)), func(ir.StreamEvent) error { return nil }, 4, 4)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func deltaEvent(index int, itemID, delta string) string {
	return fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":%q,"output_index":%d,"delta":%q}`, itemID, index, delta)
}

func wantProtocol(t *testing.T, err error, message string) {
	t.Helper()
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != message {
		t.Fatalf("err = %v, want %q", err, message)
	}
	if strings.Contains(err.Error(), "/example") || strings.Contains(err.Error(), "path") || strings.Contains(err.Error(), "{") {
		t.Fatalf("error leaked payload: %v", err)
	}
}

func assertMessageBeforeTool(t *testing.T, evs []ir.StreamEvent) {
	t.Helper()
	if len(evs) == 0 || evs[0].Kind != ir.EventMessageStart {
		t.Fatalf("first event = %+v, want MessageStart", evs)
	}
	starts := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventMessageStart {
			starts++
		}
		if ev.Kind == ir.EventToolCallStart && starts == 0 {
			t.Fatalf("tool start preceded message start: %+v", evs)
		}
	}
	if starts != 1 {
		t.Fatalf("message starts = %d, events = %+v", starts, evs)
	}
}

func TestDecodeStreamDeltaBeforeEmptyAddedKeepsPrefix(t *testing.T) {
	full := `{"path":"/example/a"}`
	t.Run("full delta then empty added", func(t *testing.T) {
		body := sseData(
			deltaEvent(0, "fc_a", full),
			addedEvent(0, "fc_a", "call_a", "read", ""),
			argsDoneEvent(0, "fc_a", "read", full),
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", full)), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertMessageBeforeTool(t, evs)
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: full, deltas: 1}})
		if finish == nil || finish.StopReason != ir.StopToolUse {
			t.Fatalf("finish = %+v", finish)
		}
	})
	t.Run("partial delta then added then snapshot", func(t *testing.T) {
		prefix := full[:8]
		body := sseData(
			deltaEvent(0, "fc_a", prefix),
			addedEvent(0, "fc_a", "call_a", "read", ""),
			argsDoneEvent(0, "fc_a", "read", full),
			completedEvent(`[]`, ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: full, deltas: 2}})
		if !strings.HasPrefix(calls[0].args, prefix) {
			t.Fatalf("held prefix lost: %q", calls[0].args)
		}
	})
}

func TestDecodeStreamArgCapCountsRetainedBytesOnce(t *testing.T) {
	err := decodeStreamLimited(strings.NewReader(sseData(
		deltaEvent(0, "fc_a", "abc"),
		addedEvent(0, "fc_a", "call_a", "read", ""),
		deltaEvent(0, "fc_a", "def"),
		completedEvent(`[]`, ""),
	)), func(ir.StreamEvent) error { return nil }, 6, 4)
	if err != nil {
		t.Fatal(err)
	}
	err = decodeStreamLimited(strings.NewReader(sseData(
		deltaEvent(0, "fc_a", "abc"),
		addedEvent(0, "fc_a", "call_a", "read", ""),
		deltaEvent(0, "fc_a", "defg"),
	)), func(ir.StreamEvent) error { return nil }, 6, 4)
	wantProtocol(t, err, "function call arguments too large")
}

func TestDecodeStreamTotalArgCapAcrossCallsAndSuffix(t *testing.T) {
	err := decodeStreamLimited(strings.NewReader(sseData(
		addedEvent(0, "fc_a", "call_a", "read", ""),
		deltaEvent(0, "fc_a", "abcd"),
		addedEvent(1, "fc_b", "call_b", "read", ""),
		argsDoneEvent(1, "fc_b", "read", "ef"),
	)), func(ir.StreamEvent) error { return nil }, 6, 4)
	if err != nil {
		t.Fatal(err)
	}
	err = decodeStreamLimited(strings.NewReader(sseData(
		addedEvent(0, "fc_a", "call_a", "read", ""),
		deltaEvent(0, "fc_a", "abcd"),
		argsDoneEvent(1, "fc_b", "read", "efg"),
	)), func(ir.StreamEvent) error { return nil }, 6, 4)
	wantProtocol(t, err, "function call arguments too large")
}

func TestDecodeStreamManySmallFragmentsKeepOrder(t *testing.T) {
	const n = 100000
	var events []string
	events = append(events, addedEvent(0, "fc_a", "call_a", "read", ""))
	var want strings.Builder
	for i := 0; i < n; i++ {
		ch := string(rune('a' + i%26))
		want.WriteString(ch)
		events = append(events, deltaEvent(0, "fc_a", ch))
	}
	events = append(events, completedEvent(`[]`, ""))
	evs, err := collectStream(t, sseData(events...))
	if err != nil {
		t.Fatal(err)
	}
	calls, finish := assembleCalls(evs)
	if len(calls) != 1 || calls[0].args != want.String() || calls[0].deltas != n {
		t.Fatalf("len=%d deltas=%d prefix=%q", len(calls), calls[0].deltas, calls[0].args[:32])
	}
	if finish == nil || finish.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestDecodeStreamArgsDoneWithoutCompletionUsesToolStop(t *testing.T) {
	args := readArgs("/example/eof.kt")
	body := sseData(argsDoneEvent(0, "fc_a", "read", args), completedEvent(`[]`, ""))
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertMessageBeforeTool(t, evs)
	calls, finish := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "", name: "read", args: args, deltas: 1}})
	if finish == nil || finish.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestDecodeStreamToolSourcesStartMessageFirst(t *testing.T) {
	args := readArgs("/example/start.kt")
	tests := []struct {
		name string
		body string
	}{
		{"added", sseData(addedEvent(0, "fc_a", "call_a", "read", args), completedEvent(`[]`, ""))},
		{"delta", sseData(addedEvent(0, "fc_a", "call_a", "read", ""), deltaEvent(0, "fc_a", args), completedEvent(`[]`, ""))},
		{"arguments done", sseData(argsDoneEvent(0, "fc_a", "read", args), itemDoneEvent(0, "fc_a", "call_a", "read", args))},
		{"item done", sseData(itemDoneEvent(0, "fc_a", "call_a", "read", args), completedEvent(`[]`, ""))},
		{"final", sseData(completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""))},
		{"pending eof", sseData(deltaEvent(0, "fc_a", args))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := collectStream(t, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			assertMessageBeforeTool(t, evs)
			calls, finish := assembleCalls(evs)
			if len(calls) != 1 || calls[0].args != args {
				t.Fatalf("calls = %+v", calls)
			}
			if finish == nil || finish.StopReason != ir.StopToolUse {
				t.Fatalf("finish = %+v", finish)
			}
		})
	}
}

func TestDecodeStreamCreatedIdentityStableWhenToolArrivesLater(t *testing.T) {
	args := readArgs("/example/id-stable.kt")
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_stable","model":"gpt-test","status":"in_progress"}}`,
		itemDoneEvent(0, "fc_a", "call_a", "read", args),
		completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Kind != ir.EventMessageStart || evs[0].ID != "resp_stable" || evs[0].Model != "gpt-test" {
		t.Fatalf("start = %+v", evs[0])
	}
	starts := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventMessageStart {
			starts++
			if ev.ID != "resp_stable" || ev.Model != "gpt-test" {
				t.Fatalf("identity changed: %+v", ev)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("starts = %d", starts)
	}
}

func TestDecodeStreamTerminalExtensionsFail(t *testing.T) {
	args := readArgs("/example/term.kt")
	tests := []struct {
		name string
		body string
	}{
		{
			"item done extension",
			sseData(
				itemDoneEvent(0, "fc_a", "call_a", "read", args),
				itemDoneEvent(0, "fc_a", "call_a", "read", args+" "),
				completedEvent(`[]`, ""),
			),
		},
		{
			"delta after arguments done",
			sseData(
				addedEvent(0, "fc_a", "call_a", "read", ""),
				argsDoneEvent(0, "fc_a", "read", args),
				deltaEvent(0, "fc_a", " "),
				completedEvent(`[]`, ""),
			),
		},
		{
			"delta after completed",
			sseData(
				addedEvent(0, "fc_a", "call_a", "read", ""),
				deltaEvent(0, "fc_a", args),
				completedEvent(`[]`, ""),
				deltaEvent(0, "fc_a", " "),
			),
		},
		{
			"delta after incomplete",
			sseData(
				addedEvent(0, "fc_a", "call_a", "read", ""),
				deltaEvent(0, "fc_a", args),
				`{"type":"response.incomplete","response":{"id":"resp_test","status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":2}}}`,
				deltaEvent(0, "fc_a", " "),
			),
		},
		{
			"shrinking snapshot after done",
			sseData(
				argsDoneEvent(0, "fc_a", "read", args),
				itemDoneEvent(0, "fc_a", "call_a", "read", args[:len(args)-1]),
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStream(t, tc.body)
			wantProtocol(t, err, "conflicting function call arguments")
		})
	}
}

func TestDecodeStreamRepeatedTerminalSnapshotsAccepted(t *testing.T) {
	args := readArgs("/example/repeat.kt")
	body := sseData(
		addedEvent(0, "fc_a", "call_a", "read", ""),
		argsDoneEvent(0, "fc_a", "read", args),
		argsDoneEvent(0, "fc_a", "read", args),
		itemDoneEvent(0, "fc_a", "call_a", "read", args),
		itemDoneEvent(0, "fc_a", "call_a", "read", args),
		completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
		completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, finish := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 1}})
	if finish == nil || finish.StopReason != ir.StopToolUse || finish.InputTokens != 11 {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestDecodeStreamAddedSnapshotStaysOpen(t *testing.T) {
	args := readArgs("/example/open.kt")
	body := sseData(
		addedEvent(0, "fc_a", "call_a", "read", args),
		deltaEvent(0, "fc_a", " "),
		itemDoneEvent(0, "fc_a", "call_a", "read", args+" "),
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := assembleCalls(evs)
	assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args + " "}})
}

func TestDecodeStreamArgsDoneHoldsUntilIdentity(t *testing.T) {
	args := readArgs("/example/hold.kt")
	body := sseData(
		argsDoneEvent(3, "fc_hold", "read", args),
		itemDoneEvent(3, "fc_hold", "call_hold", "read", args),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var sawStart bool
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventToolCallStart:
			if ev.ToolID != "call_hold" || ev.ToolName != "read" {
				t.Fatalf("start = %+v", ev)
			}
			sawStart = true
		case ir.EventToolCallDelta:
			if !sawStart {
				t.Fatal("arguments emitted before call identity")
			}
			if ev.ArgsFrag != args {
				t.Fatalf("delta = %q", ev.ArgsFrag)
			}
		}
	}
	if !sawStart {
		t.Fatalf("events = %+v", evs)
	}
}

func TestDecodeStreamFinalMatching(t *testing.T) {
	a := readArgs("/example/final-a")
	b := readArgs("/example/final-b")
	t.Run("distinct final-only names stay separate", func(t *testing.T) {
		body := sseData(completedEvent(finalOutput(
			completedItem("fc_a", "call_a", "read", a),
			completedItem("fc_b", "call_b", "read", b),
		), ""))
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{
			{index: 0, id: "call_a", name: "read", args: a},
			{index: 1, id: "call_b", name: "read", args: b},
		})
	})
	t.Run("id-less final-only ordinal", func(t *testing.T) {
		body := sseData(completedEvent(finalOutput(
			`{"type":"function_call","name":"read","arguments":"{\"path\":\"/example/only\"}"}`,
		), ""))
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, name: "read", args: `{"path":"/example/only"}`}})
	})
	t.Run("id-less final does not rebind streamed index", func(t *testing.T) {
		body := sseData(
			addedEvent(2, "fc_a", "call_a", "read", ""),
			deltaEvent(2, "fc_a", a),
			completedEvent(finalOutput(
				`{"type":"function_call","name":"read","arguments":`+fmt.Sprintf("%q", a)+`}`,
			), ""),
		)
		_, err := collectStream(t, body)
		wantProtocol(t, err, "conflicting function call identity")
	})
	t.Run("exact id-less ordinal reconciles", func(t *testing.T) {
		body := sseData(
			addedEvent(0, "fc_a", "call_a", "read", ""),
			deltaEvent(0, "fc_a", a),
			completedEvent(finalOutput(
				`{"type":"function_call","name":"read","arguments":`+fmt.Sprintf("%q", a)+`}`,
			), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: a, deltas: 1}})
	})
	t.Run("same name different ids stay separate", func(t *testing.T) {
		body := sseData(
			addedEvent(2, "fc_a", "call_a", "read", ""),
			addedEvent(4, "fc_b", "call_b", "read", ""),
			deltaEvent(2, "fc_a", a),
			deltaEvent(4, "fc_b", b),
			completedEvent(finalOutput(
				completedItem("fc_a", "call_a", "read", a),
				completedItem("fc_b", "call_b", "read", b),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{
			{index: 0, id: "call_a", name: "read", args: a},
			{index: 1, id: "call_b", name: "read", args: b},
		})
	})
}

func TestDecodeStreamIncompleteToolStopStaysMaxTokens(t *testing.T) {
	args := readArgs("/example/max.kt")
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_max","model":"gpt-test","status":"in_progress"}}`,
		itemDoneEvent(0, "fc_a", "call_a", "read", args),
		`{"type":"response.incomplete","response":{"id":"resp_max","model":"gpt-test","status":"incomplete","output":[],"usage":{"input_tokens":4,"output_tokens":8,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"cached_tokens":2}}}}`,
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	_, finish := assembleCalls(evs)
	if finish == nil || finish.StopReason != ir.StopMaxTokens || finish.InputTokens != 4 || finish.OutputTokens != 8 {
		t.Fatalf("finish = %+v", finish)
	}
	if evs[0].ID != "resp_max" || evs[0].Model != "gpt-test" {
		t.Fatalf("start = %+v", evs[0])
	}
}

func indexDelta(index int, args string) string {
	return fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":%d,"delta":%q}`, index, args)
}

func idLessFinal(name, args string) string {
	return fmt.Sprintf(`{"type":"function_call","name":%q,"arguments":%q}`, name, args)
}

func itemDoneNoCallID(index int, id, name, args string) string {
	item := fmt.Sprintf(`{"id":%q,"type":"function_call","status":"completed","name":%q,"arguments":%q}`, id, name, args)
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item)
}

func incompleteEvent(output string) string {
	return fmt.Sprintf(`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","output":%s}}`, output)
}

func assertFinish(t *testing.T, finish *ir.StreamEvent, stop ir.StopReason) {
	t.Helper()
	if finish == nil || finish.StopReason != stop {
		t.Fatalf("finish = %+v, want %s", finish, stop)
	}
}

func TestDecodeStreamRejectsNewWorkAfterResponseTerminal(t *testing.T) {
	args := `{"path":"/example/a"}`
	t.Run("delta after completed empty output", func(t *testing.T) {
		_, err := collectStream(t, sseData(completedEvent(`[]`, ""), indexDelta(9, args)))
		wantProtocol(t, err, "conflicting function call identity")
	})
	t.Run("identified tool after incomplete", func(t *testing.T) {
		_, err := collectStream(t, sseData(incompleteEvent(`[]`), addedEvent(0, "fc_a", "call_a", "read", "")))
		wantProtocol(t, err, "conflicting function call identity")
	})
	t.Run("text after completed", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			completedEvent(`[]`, ""),
			`{"type":"response.output_text.delta","delta":"late"}`,
		))
		wantProtocol(t, err, "conflicting response content")
	})
	t.Run("reasoning after completed", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			completedEvent(`[]`, ""),
			`{"type":"response.reasoning_summary_text.delta","delta":"late"}`,
		))
		wantProtocol(t, err, "conflicting response content")
	})
	t.Run("repeat completion with unknown item", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			completedEvent(`[]`, ""),
			completedEvent(finalOutput(completedItem("fc_new", "call_new", "read", args)), ""),
		))
		wantProtocol(t, err, "conflicting function call identity")
	})
	t.Run("repeat known completion stays one call", func(t *testing.T) {
		body := sseData(
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 1}})
		assertFinish(t, finish, ir.StopToolUse)
	})
	t.Run("empty text delta after completed is ignored", func(t *testing.T) {
		evs, err := collectStream(t, sseData(
			`{"type":"response.output_text.delta","delta":"ok"}`,
			completedEvent(`[]`, ""),
			`{"type":"response.output_text.delta","delta":""}`,
		))
		if err != nil {
			t.Fatal(err)
		}
		if _, finish := assembleCalls(evs); finish == nil || finish.StopReason != ir.StopEndTurn {
			t.Fatalf("finish = %+v", finish)
		}
	})
	t.Run("failed status still precedes terminal content", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			`{"type":"response.completed","response":{"id":"r1","status":"failed","error":{"code":"429","message":"nope"}}}`,
		))
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Code != "429" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDecodeStreamFinalOnlyCallsStaySeparate(t *testing.T) {
	empty := "{}"
	a := `{"path":"/example/a"}`
	b := `{"path":"/example/b"}`
	c := `{"path":"/example/c"}`
	d := `{"path":"/example/d"}`
	t.Run("two same-name id-less calls", func(t *testing.T) {
		body := sseData(completedEvent(finalOutput(idLessFinal("read", empty), idLessFinal("read", empty)), ""))
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertMessageBeforeTool(t, evs)
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{
			{index: 0, name: "read", args: empty, deltas: 1},
			{index: 1, name: "read", args: empty, deltas: 1},
		})
		assertFinish(t, finish, ir.StopToolUse)
		for _, call := range calls {
			if call.id != "" {
				t.Fatalf("guessed call id: %+v", calls)
			}
		}
	})
	t.Run("distinct names and arguments", func(t *testing.T) {
		body := sseData(completedEvent(finalOutput(idLessFinal("read", a), idLessFinal("bash", b)), ""))
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{
			{index: 0, name: "read", args: a},
			{index: 1, name: "bash", args: b},
		})
		assertFinish(t, finish, ir.StopToolUse)
	})
	t.Run("three and four id-less calls", func(t *testing.T) {
		for _, items := range [][]string{
			{idLessFinal("read", a), idLessFinal("read", b), idLessFinal("read", c)},
			{idLessFinal("read", a), idLessFinal("read", b), idLessFinal("read", c), idLessFinal("read", d)},
		} {
			evs, err := collectStream(t, sseData(completedEvent(finalOutput(items...), "")))
			if err != nil {
				t.Fatal(err)
			}
			calls, finish := assembleCalls(evs)
			if len(calls) != len(items) || finish == nil || finish.StopReason != ir.StopToolUse {
				t.Fatalf("calls=%+v finish=%+v", calls, finish)
			}
			wantArgs := []string{a, b, c, d}
			for i, call := range calls {
				if call.id != "" || call.name != "read" || call.args != wantArgs[i] || call.index != i {
					t.Fatalf("call %d = %+v", i, call)
				}
			}
		}
	})
	t.Run("mixed id and id-less final-only", func(t *testing.T) {
		body := sseData(completedEvent(finalOutput(
			completedItem("fc_a", "call_a", "read", a),
			idLessFinal("read", b),
			completedItem("", "call_c", "read", c),
		), ""))
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{
			{index: 0, id: "call_a", name: "read", args: a},
			{index: 1, name: "read", args: b},
			{index: 2, id: "call_c", name: "read", args: c},
		})
	})
	t.Run("repeated identical id-less snapshot", func(t *testing.T) {
		output := finalOutput(idLessFinal("read", empty), idLessFinal("read", a))
		evs, err := collectStream(t, sseData(completedEvent(output, ""), completedEvent(output, "")))
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{
			{index: 0, name: "read", args: empty, deltas: 1},
			{index: 1, name: "read", args: a, deltas: 1},
		})
	})
	t.Run("changed id-less snapshot conflicts", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			completedEvent(finalOutput(idLessFinal("read", a), idLessFinal("read", b)), ""),
			completedEvent(finalOutput(idLessFinal("read", a), idLessFinal("read", c)), ""),
		))
		wantProtocol(t, err, "conflicting function call arguments")
	})
	t.Run("known streamed call plus distinct final-only call", func(t *testing.T) {
		body := sseData(
			addedEvent(2, "fc_a", "call_a", "read", ""),
			deltaEvent(2, "fc_a", a),
			completedEvent(finalOutput(
				completedItem("fc_a", "call_a", "read", a),
				completedItem("fc_b", "call_b", "bash", b),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{
			{index: 0, id: "call_a", name: "read", args: a, deltas: 1},
			{index: 1, id: "call_b", name: "bash", args: b, deltas: 1},
		})
		assertFinish(t, finish, ir.StopToolUse)
	})
}

func TestDecodeStreamReleasesUnstartedEmptyCalls(t *testing.T) {
	t.Run("arguments done then eof", func(t *testing.T) {
		evs, err := collectStream(t, sseData(argsDoneEvent(0, "fc_a", "read", "")))
		if err != nil {
			t.Fatal(err)
		}
		assertMessageBeforeTool(t, evs)
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, name: "read", args: ""}})
		assertFinish(t, finish, ir.StopToolUse)
		if calls[0].id != "" {
			t.Fatalf("invented call id: %+v", calls[0])
		}
	})
	t.Run("empty completion array still releases pending call", func(t *testing.T) {
		evs, err := collectStream(t, sseData(argsDoneEvent(0, "fc_a", "read", ""), completedEvent(`[]`, "")))
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, name: "read"}})
		assertFinish(t, finish, ir.StopToolUse)
	})
	t.Run("index-only zero delta is released", func(t *testing.T) {
		evs, err := collectStream(t, sseData(indexDelta(4, "")))
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0}})
		assertFinish(t, finish, ir.StopToolUse)
		starts := 0
		for _, ev := range evs {
			if ev.Kind == ir.EventToolCallStart {
				starts++
			}
		}
		if starts != 1 {
			t.Fatalf("starts = %d, events = %+v", starts, evs)
		}
	})
}

func TestDecodeStreamFinalOnlyEmptyArguments(t *testing.T) {
	t.Run("id-less empty arguments", func(t *testing.T) {
		evs, err := collectStream(t, sseData(completedEvent(finalOutput(idLessFinal("read", "")), "")))
		if err != nil {
			t.Fatal(err)
		}
		assertMessageBeforeTool(t, evs)
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, name: "read", args: ""}})
		assertFinish(t, finish, ir.StopToolUse)
	})
	t.Run("item id without call id", func(t *testing.T) {
		item := `{"id":"fc_a","type":"function_call","name":"read","arguments":""}`
		evs, err := collectStream(t, sseData(completedEvent(finalOutput(item), "")))
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, name: "read"}})
		assertFinish(t, finish, ir.StopToolUse)
		if calls[0].id != "" {
			t.Fatalf("guessed call id: %+v", calls[0])
		}
	})
	t.Run("item done without call id", func(t *testing.T) {
		evs, err := collectStream(t, sseData(itemDoneNoCallID(1, "fc_a", "read", "")))
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, name: "read"}})
		assertFinish(t, finish, ir.StopToolUse)
	})
	t.Run("incomplete empty pending stays max tokens", func(t *testing.T) {
		evs, err := collectStream(t, sseData(
			argsDoneEvent(0, "fc_a", "read", ""),
			incompleteEvent(`[]`),
		))
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, name: "read"}})
		assertFinish(t, finish, ir.StopMaxTokens)
	})
}

func TestDecodeStreamAmbiguousFinalIdentity(t *testing.T) {
	args := `{"path":"/example/a"}`
	t.Run("index-only delta then different final identity", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			indexDelta(7, args),
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
		))
		wantProtocol(t, err, "conflicting function call identity")
	})
	t.Run("different item id is not the same call", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			deltaEvent(7, "fc_x", args),
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
		))
		wantProtocol(t, err, "conflicting function call identity")
	})
	t.Run("exact index fills identity", func(t *testing.T) {
		evs, err := collectStream(t, sseData(
			indexDelta(0, args),
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
		))
		if err != nil {
			t.Fatal(err)
		}
		calls, finish := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 1}})
		assertFinish(t, finish, ir.StopToolUse)
	})
	t.Run("same item id fills call id", func(t *testing.T) {
		evs, err := collectStream(t, sseData(
			deltaEvent(7, "fc_a", args),
			completedEvent(finalOutput(completedItem("fc_a", "call_a", "read", args)), ""),
		))
		if err != nil {
			t.Fatal(err)
		}
		calls, _ := assembleCalls(evs)
		assertCalls(t, calls, []decodedCall{{index: 0, id: "call_a", name: "read", args: args, deltas: 1}})
	})
	t.Run("call id conflict on exact index", func(t *testing.T) {
		_, err := collectStream(t, sseData(
			addedEvent(0, "fc_a", "call_a", "read", ""),
			deltaEvent(0, "fc_a", args),
			completedEvent(finalOutput(completedItem("fc_a", "call_b", "read", args)), ""),
		))
		wantProtocol(t, err, "conflicting function call id")
	})
}

func TestDecodeResponseFailedNonStringMetadata(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantCode    string
		wantType    string
		wantMessage string
	}{
		{
			name:        "numeric code",
			body:        `{"id":"r1","model":"m","status":"failed","error":{"code":429,"message":"overloaded"},"output":[],"usage":null}`,
			wantCode:    "429",
			wantMessage: "overloaded",
		},
		{
			name:        "null code",
			body:        `{"id":"r1","model":"m","status":"failed","error":{"code":null,"type":"server_error","message":"overloaded"},"output":[],"usage":null}`,
			wantType:    "server_error",
			wantMessage: "overloaded",
		},
		{
			name:        "unsupported metadata",
			body:        `{"id":"r1","model":"m","status":"failed","error":{"code":true,"type":{"n":1},"message":["x"]},"output":[],"usage":null}`,
			wantMessage: "upstream response failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := DecodeResponse([]byte(tc.body))
			if resp != nil {
				t.Fatalf("want no ordinary response, got %+v", resp)
			}
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
		})
	}
}
