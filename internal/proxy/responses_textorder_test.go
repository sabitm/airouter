package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/responses"
)

// TestResponsesDecodeToCollectorTextOrder runs the real decode stream into the
// proxy collector so the per-message recovered suffix order is asserted on the
// collected text, not only on the final object.
func TestResponsesDecodeToCollectorTextOrder(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","item_id":"msg_a","delta":"hel"}

data: {"type":"response.output_text.delta","item_id":"msg_b","delta":"world"}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"id":"msg_b","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}

`
	rec := httptest.NewRecorder()
	sink := &translatedSink{w: rec, res: &reqResult{}, enc: responsesCodec.newStreamEncoder("m"), ingressID: "oai-chat"}
	if err := responses.DecodeStream(strings.NewReader(body), sink.handle); err != nil {
		t.Fatal(err)
	}
	var text string
	for _, b := range sink.response().Content {
		if b.Type == ir.BlockText {
			text += b.Text
		}
	}
	if text != "helloworld" {
		t.Fatalf("collector text = %q, want helloworld", text)
	}
}

// TestResponsesDecodeToActualCollectorMixedToolOrder runs the mixed message and
// streamed tool stream through the real collector, so the recovered message
// suffix precedes the tool in the collected content and tool list.
func TestResponsesDecodeToActualCollectorMixedToolOrder(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","item_id":"msg_a","delta":"hel"}

data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_1","name":"read","arguments":""}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"path\":"}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"/x\"}"}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"read","arguments":"{\"path\":\"/x\"}"}],"usage":{"input_tokens":1,"output_tokens":1}}}

`
	resp, err := collectStreamResponse(strings.NewReader(body), responsesCodec, nil, "m", nil)
	if err != nil {
		t.Fatalf("collect real Responses stream: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %+v, want text then tool_use", resp.Content)
	}
	if resp.Content[0].Type != ir.BlockText || resp.Content[0].Text != "hello" {
		t.Fatalf("content[0] = %+v, want hello", resp.Content[0])
	}
	if resp.Content[1].Type != ir.BlockToolUse || resp.Content[1].ToolName != "read" {
		t.Fatalf("content[1] = %+v, want read tool", resp.Content[1])
	}
	if string(resp.Content[1].ToolInput) != `{"path":"/x"}` {
		t.Fatalf("tool input = %s", resp.Content[1].ToolInput)
	}
}

// decode into the real stream-to-unary collector, not the translatedSink usage
// estimator. A late part-zero done after later-part bytes must keep the full
// multipart text in order.
func TestResponsesDecodeToActualCollectorMultipartText(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","item_id":"msg_a","output_index":0,"content_index":0,"delta":"part1"}

data: {"type":"response.output_text.delta","item_id":"msg_a","output_index":0,"content_index":1,"delta":"part2"}

data: {"type":"response.output_text.done","item_id":"msg_a","output_index":0,"content_index":0,"text":"part1"}

`
	resp, err := collectStreamResponse(strings.NewReader(body), responsesCodec, nil, "m", nil)
	if err != nil {
		t.Fatalf("collect real Responses stream: %v", err)
	}
	var text string
	for _, b := range resp.Content {
		if b.Type == ir.BlockText {
			text += b.Text
		}
	}
	if text != "part1part2" {
		t.Fatalf("collector text = %q, want part1part2", text)
	}
}

// TestResponsesDecodeToActualCollectorTextPartGrowth runs the empty-placeholder
// growth sequence through the real collector. The earlier part grows after a
// later empty part done, and the collector must see the text, not panic.
func TestResponsesDecodeToActualCollectorTextPartGrowth(t *testing.T) {
	body := `data: {"type":"response.output_text.done","item_id":"msg_a","output_index":0,"content_index":1,"text":""}

data: {"type":"response.output_text.done","item_id":"msg_a","output_index":0,"content_index":0,"text":"a"}

data: {"type":"response.output_text.done","item_id":"msg_a","output_index":0,"content_index":1,"text":""}

`
	var (
		resp *ir.Response
		err  error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("collector panicked: %v", r)
			}
		}()
		resp, err = collectStreamResponse(strings.NewReader(body), responsesCodec, nil, "m", nil)
	}()
	if err != nil {
		t.Fatalf("collect real Responses stream: %v", err)
	}
	var text string
	for _, b := range resp.Content {
		if b.Type == ir.BlockText {
			text += b.Text
		}
	}
	if text != "a" {
		t.Fatalf("collector text = %q, want a", text)
	}
}

// TestResponsesDecodeToActualCollectorClosedLaterMessage keeps a closed later
// message behind an open earlier message through the real collector, so the
// recovered suffix appends after the earlier message, not before it.
func TestResponsesDecodeToActualCollectorClosedLaterMessage(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","item_id":"msg_a","output_index":0,"content_index":0,"delta":"hel"}

data: {"type":"response.output_text.delta","item_id":"msg_b","output_index":1,"content_index":0,"delta":"world"}

data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_b","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"world"}]}}

data: {"type":"response.output_text.done","item_id":"msg_a","output_index":0,"content_index":0,"text":"hel"}

data: {"type":"response.output_text.delta","item_id":"msg_a","output_index":0,"content_index":1,"delta":"lo"}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hel"},{"type":"output_text","text":"lo"}]}}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hel"},{"type":"output_text","text":"lo"}]},{"id":"msg_b","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}

`
	resp, err := collectStreamResponse(strings.NewReader(body), responsesCodec, nil, "m", nil)
	if err != nil {
		t.Fatalf("collect real Responses stream: %v", err)
	}
	var text string
	for _, b := range resp.Content {
		if b.Type == ir.BlockText {
			text += b.Text
		}
	}
	if text != "helloworld" {
		t.Fatalf("collector text = %q, want helloworld", text)
	}
}

// TestResponsesDecodeToCollectorFinalOrderingGuard runs a terminal reasoning
// item that would land before already-emitted message text through the real
// Responses decoder into the collector. The unrecoverable order must fail
// value-free before any reasoning frame is collected, and no response may be
// returned.
func TestResponsesDecodeToCollectorFinalOrderingGuard(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","item_id":"msg_a","delta":"hello"}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"R"}]},{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}

`
	resp, err := collectStreamResponse(strings.NewReader(body), responsesCodec, nil, "m", nil)
	if err == nil || !strings.Contains(err.Error(), "conflicting message text order") {
		t.Fatalf("err = %v, want conflicting message text order", err)
	}
	if resp != nil {
		t.Fatalf("collector returned a response on reorder: %+v", resp)
	}
}
