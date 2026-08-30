package openai

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestRequestReasoningRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		field string
		want  string
	}{
		{name: "reasoning content", field: `"reasoning_content":"chain"`, want: "chain"},
		{name: "reasoning alias", field: `"reasoning":"chain"`, want: "chain"},
		{name: "reasoning details", field: `"reasoning_details":[{"text":"first "},{"content":"second"}]`, want: "first second"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"assistant","content":"answer",` + tc.field + `},{"role":"user","content":"next"}]}`
			req, err := DecodeRequest([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if len(req.Messages) != 2 || len(req.Messages[0].Content) != 2 {
				t.Fatalf("messages = %+v", req.Messages)
			}
			if got := req.Messages[0].Content[0]; got.Type != ir.BlockReasoning || got.Text != tc.want {
				t.Fatalf("reasoning = %+v, want %q", got, tc.want)
			}
			if got := req.Messages[0].Content[1]; got.Type != ir.BlockText || got.Text != "answer" {
				t.Fatalf("text = %+v", got)
			}

			out, err := EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var encoded chatRequest
			if err := json.Unmarshal(out, &encoded); err != nil {
				t.Fatal(err)
			}
			if got := encoded.Messages[0].ReasoningContent; got != tc.want {
				t.Fatalf("reasoning_content = %q, want %q; body=%s", got, tc.want, out)
			}
		})
	}
}

func TestResponseReasoningRoundTrip(t *testing.T) {
	body := `{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_details":[{"text":"chain"}]},"finish_reason":"stop"}]}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != ir.BlockReasoning || resp.Content[0].Text != "chain" || resp.Content[1].Type != ir.BlockText {
		t.Fatalf("content = %+v", resp.Content)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var encoded chatResponse
	if err := json.Unmarshal(out, &encoded); err != nil {
		t.Fatal(err)
	}
	if got := encoded.Choices[0].Message.ReasoningContent; got != "chain" {
		t.Fatalf("reasoning_content = %q; body=%s", got, out)
	}
}

func TestStreamReasoningRoundTrip(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_details\":[{\"text\":\"chain\"}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var events []ir.StreamEvent
	if err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Kind != ir.EventMessageStart || events[1].Kind != ir.EventReasoningDelta || events[1].Text != "chain" || events[2].Kind != ir.EventFinish {
		t.Fatalf("events = %+v", events)
	}

	rec := httptest.NewRecorder()
	writer, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range events {
		if err := enc.Encode(ev, writer); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(writer); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_content":"chain"`) {
		t.Fatalf("encoded stream missing reasoning_content: %s", rec.Body.String())
	}
}
