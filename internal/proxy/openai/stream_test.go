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
