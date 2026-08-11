package responses

import (
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
