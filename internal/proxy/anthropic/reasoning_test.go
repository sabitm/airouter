package anthropic

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestRequestReasoningRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"chain"},{"type":"text","text":"answer"}]},{"role":"user","content":"next"}]}`
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
	var encoded messagesRequest
	if err := json.Unmarshal(out, &encoded); err != nil {
		t.Fatal(err)
	}
	var blocks []anthBlock
	if err := json.Unmarshal(encoded.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0].Type != "thinking" || blocks[0].Thinking != "chain" {
		t.Fatalf("blocks = %+v; body=%s", blocks, out)
	}
}

func TestResponseReasoningRoundTrip(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"chain"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`
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
	var encoded messagesResponse
	if err := json.Unmarshal(out, &encoded); err != nil {
		t.Fatal(err)
	}
	if len(encoded.Content) != 2 || encoded.Content[0].Type != "thinking" || encoded.Content[0].Thinking != "chain" {
		t.Fatalf("content = %+v; body=%s", encoded.Content, out)
	}
}

func TestStreamReasoningRoundTrip(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":2}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"chain"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

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
	if !strings.Contains(stream, `"content_block":{"thinking":"","type":"thinking"}`) || !strings.Contains(stream, `"delta":{"thinking":"chain","type":"thinking_delta"}`) {
		t.Fatalf("encoded stream missing thinking events: %s", stream)
	}
}
