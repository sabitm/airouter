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
