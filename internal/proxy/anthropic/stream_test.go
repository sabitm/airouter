package anthropic

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func collectDecode(t *testing.T, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

func TestDecodeStreamEmptyYieldsNoEvents(t *testing.T) {
	// Empty / comment / ping / usage-only streams must not fabricate Finish.
	// message_delta updates stop/usage but emits no IR, so it is the same branch.
	bodies := []string{
		"",
		": ping\n\n",
		"event: ping\ndata: {}\n\n",
		"\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":9}}\n\n",
	}
	for _, body := range bodies {
		events, err := collectDecode(t, body)
		if err != nil {
			t.Errorf("body %q: %v", body, err)
			continue
		}
		if len(events) != 0 {
			t.Errorf("body %q emitted %+v, want no events", body, events)
		}
	}
}

func TestDecodeStreamDeltaWithoutStartSynthesizesFinish(t *testing.T) {
	body := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != ir.EventTextDelta || events[0].Text != "hi" || events[1].Kind != ir.EventFinish {
		t.Fatalf("events = %+v, want TextDelta(hi) + Finish", events)
	}
}

func TestDecodeStreamStartWithoutStopSynthesizesFinish(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"up\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != ir.EventMessageStart || events[0].ID != "msg_1" || events[1].Kind != ir.EventFinish {
		t.Fatalf("events = %+v, want MessageStart + Finish", events)
	}
	if events[1].InputTokens != 3 {
		t.Errorf("finish input = %d, want 3", events[1].InputTokens)
	}
}

func TestDecodeStreamErrorEvent(t *testing.T) {
	body := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Type != "overloaded_error" {
		t.Errorf("type = %q", sf.Type)
	}
	if sf.Message != "Overloaded" {
		t.Errorf("message = %q", sf.Message)
	}
	for _, ev := range out {
		if ev.Kind == ir.EventFinish {
			t.Fatal("must not emit Finish on error")
		}
	}
}

func encodeStream(t *testing.T, events []ir.StreamEvent) string {
	t.Helper()
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
	return rec.Body.String()
}

func collectAnthropicUsageFrames(t *testing.T, body string) (startIn int, deltaIn *int, deltaOut int) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	startIn = -1
	deltaOut = -1
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "message_start":
			var d struct {
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				t.Fatalf("message_start: %s", ev.Data)
			}
			startIn = d.Message.Usage.InputTokens
		case "message_delta":
			var d struct {
				Usage struct {
					InputTokens  *int `json:"input_tokens"`
					OutputTokens int  `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				t.Fatalf("message_delta: %s", ev.Data)
			}
			deltaIn = d.Usage.InputTokens
			deltaOut = d.Usage.OutputTokens
		}
	}
	return startIn, deltaIn, deltaOut
}

func TestEncodeStreamLateInputOnFinish(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m", InputTokens: 0},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 12, OutputTokens: 3},
	})
	startIn, deltaIn, deltaOut := collectAnthropicUsageFrames(t, body)
	if startIn != 0 {
		t.Fatalf("message_start input_tokens = %d, want 0", startIn)
	}
	if deltaIn == nil || *deltaIn != 12 {
		t.Fatalf("message_delta input_tokens = %v, want 12", deltaIn)
	}
	if deltaOut != 3 {
		t.Fatalf("message_delta output_tokens = %d, want 3", deltaOut)
	}
}

func TestEncodeStreamKnownInputNotRepeatedOnDelta(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m", InputTokens: 12},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 12, OutputTokens: 3},
	})
	startIn, deltaIn, deltaOut := collectAnthropicUsageFrames(t, body)
	if startIn != 12 {
		t.Fatalf("message_start input_tokens = %d, want 12", startIn)
	}
	if deltaIn != nil {
		t.Fatalf("message_delta input_tokens = %v, want omitted", deltaIn)
	}
	if deltaOut != 3 {
		t.Fatalf("message_delta output_tokens = %d, want 3", deltaOut)
	}
}

func TestEncodeStreamFinishFirstPutsInputOnStart(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 12, OutputTokens: 3},
	})
	startIn, deltaIn, deltaOut := collectAnthropicUsageFrames(t, body)
	if startIn != 12 {
		t.Fatalf("message_start input_tokens = %d, want 12", startIn)
	}
	if deltaIn != nil {
		t.Fatalf("message_delta input_tokens = %v, want omitted", deltaIn)
	}
	if deltaOut != 3 {
		t.Fatalf("message_delta output_tokens = %d, want 3", deltaOut)
	}
}
