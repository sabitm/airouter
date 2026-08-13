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
