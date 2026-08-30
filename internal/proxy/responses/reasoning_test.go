package responses

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestRequestReasoningRoundTrip(t *testing.T) {
	body := `{"model":"m","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"chain"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 || len(req.Messages[0].Content) != 2 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if got := req.Messages[0].Content[0]; got.Type != ir.BlockReasoning || got.Text != "chain" {
		t.Fatalf("reasoning = %+v", got)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		Input []inputItem `json:"input"`
	}
	if err := json.Unmarshal(out, &encoded); err != nil {
		t.Fatal(err)
	}
	if len(encoded.Input) < 2 || encoded.Input[0].Type != "reasoning" || len(encoded.Input[0].Summary) != 1 || encoded.Input[0].Summary[0].Text != "chain" {
		t.Fatalf("encoded input = %+v; body=%s", encoded.Input, out)
	}
}

func TestResponseReasoningRoundTrip(t *testing.T) {
	body := `{"id":"resp_1","model":"m","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"chain"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":2,"output_tokens":3}}`
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
	var encoded respObject
	if err := json.Unmarshal(out, &encoded); err != nil {
		t.Fatal(err)
	}
	if len(encoded.Output) != 2 || encoded.Output[0].Type != "reasoning" || encoded.Output[0].Summary[0].Text != "chain" {
		t.Fatalf("output = %+v; body=%s", encoded.Output, out)
	}
}

func TestStreamReasoningRoundTrip(t *testing.T) {
	body := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"chain"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","usage":{"input_tokens":2,"output_tokens":3}}}

`
	var events []ir.StreamEvent
	if err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1].Kind != ir.EventReasoningDelta || events[1].Text != "chain" {
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
	stream := rec.Body.String()
	for _, event := range []string{"response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done"} {
		if !strings.Contains(stream, event) {
			t.Fatalf("encoded stream missing %s: %s", event, stream)
		}
	}
	if !strings.Contains(stream, `"type":"reasoning"`) || !strings.Contains(stream, `"text":"chain"`) {
		t.Fatalf("completed reasoning item missing: %s", stream)
	}
}
