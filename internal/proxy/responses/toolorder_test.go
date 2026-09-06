package responses

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func encodeStreamEvents(t *testing.T, model string, events []ir.StreamEvent) string {
	t.Helper()
	enc := NewStreamEncoder(model)
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("sse writer")
	}
	for _, ev := range events {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(w); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String()
}

type respFrame struct {
	Name string
	Data map[string]any
}

func parseResponsesStream(t *testing.T, body string) []respFrame {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var frames []respFrame
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		frames = append(frames, respFrame{Name: ev.Name, Data: data})
	}
	return frames
}

func reassembleResponsesTools(t *testing.T, frames []respFrame) map[int]map[string]string {
	t.Helper()
	calls := map[int]map[string]string{}
	for _, f := range frames {
		switch f.Name {
		case "response.output_item.added":
			item, _ := f.Data["item"].(map[string]any)
			if item["type"] != "function_call" {
				continue
			}
			idx := int(f.Data["output_index"].(float64))
			call, ok := calls[idx]
			if !ok {
				call = map[string]string{}
				calls[idx] = call
			}
			if id, ok := item["call_id"].(string); ok && id != "" {
				call["id"] = id
			}
			if name, ok := item["name"].(string); ok && name != "" {
				call["name"] = name
			}
		case "response.function_call_arguments.delta":
			idx := int(f.Data["output_index"].(float64))
			call, ok := calls[idx]
			if !ok {
				call = map[string]string{}
				calls[idx] = call
			}
			call["args"] += f.Data["delta"].(string)
		case "response.function_call_arguments.done":
			idx := int(f.Data["output_index"].(float64))
			call, ok := calls[idx]
			if !ok {
				call = map[string]string{}
				calls[idx] = call
			}
			if args, ok := f.Data["arguments"].(string); ok {
				call["done"] = args
			}
		}
	}
	return calls
}

func assertResponsesWireOrder(t *testing.T, frames []respFrame) {
	t.Helper()
	sawCreated := false
	openItems := map[string]bool{}
	for i, f := range frames {
		switch f.Name {
		case "response.created":
			sawCreated = true
		case "response.output_item.added":
			if !sawCreated {
				t.Fatalf("output_item.added before response.created at frame %d", i)
			}
			item, _ := f.Data["item"].(map[string]any)
			id, _ := item["id"].(string)
			if id == "" {
				t.Fatalf("empty item id at frame %d", i)
			}
			if openItems[id] {
				t.Errorf("duplicate output_item.added for %s at frame %d", id, i)
			}
			openItems[id] = true
		case "response.function_call_arguments.delta":
			if !sawCreated {
				t.Fatalf("function_call_arguments.delta before response.created at frame %d", i)
			}
			id, _ := f.Data["item_id"].(string)
			if id == "" || !openItems[id] {
				t.Fatalf("delta for unknown item_id %q at frame %d", id, i)
			}
		}
	}
}

func oneCall(t *testing.T, calls map[int]map[string]string) map[string]string {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("want one function call, got %d: %v", len(calls), calls)
	}
	for _, c := range calls {
		return c
	}
	return nil
}

func TestEncodeStreamDeltaBeforeStartKeepsArgs(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `{"ci`},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "call_1", ToolName: "get_weather"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `ty":"SF"}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	frames := parseResponsesStream(t, body)
	assertResponsesWireOrder(t, frames)
	call := oneCall(t, reassembleResponsesTools(t, frames))
	if call["id"] != "call_1" || call["name"] != "get_weather" {
		t.Errorf("identity = %v", call)
	}
	if call["args"] != `{"city":"SF"}` {
		t.Errorf("args = %q, want %q", call["args"], `{"city":"SF"}`)
	}
	if call["done"] != `{"city":"SF"}` {
		t.Errorf("done snapshot = %q, want %q", call["done"], `{"city":"SF"}`)
	}
}

func TestEncodeStreamSplitIdentityAcrossStartsKeepsArgs(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1"},
		{Kind: ir.EventToolCallStart, Index: 0, ToolName: "get_weather"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"city":"SF"}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	frames := parseResponsesStream(t, body)
	assertResponsesWireOrder(t, frames)
	call := oneCall(t, reassembleResponsesTools(t, frames))
	if call["id"] != "call_1" || call["name"] != "get_weather" {
		t.Errorf("identity = %v", call)
	}
	if call["args"] != `{"city":"SF"}` {
		t.Errorf("args = %q, want %q", call["args"], `{"city":"SF"}`)
	}
	if call["done"] != `{"city":"SF"}` {
		t.Errorf("done snapshot = %q, want %q", call["done"], `{"city":"SF"}`)
	}
}

func TestEncodeStreamInterleavedParallelCalls(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_a", ToolName: "alpha"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `{"x":`},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "call_b", ToolName: "beta"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"a":1}`},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `2}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	frames := parseResponsesStream(t, body)
	assertResponsesWireOrder(t, frames)
	calls := reassembleResponsesTools(t, frames)
	if len(calls) != 2 {
		t.Fatalf("want two function calls, got %d: %v", len(calls), calls)
	}
	if calls[0]["id"] != "call_a" || calls[0]["args"] != `{"a":1}` || calls[0]["done"] != `{"a":1}` {
		t.Errorf("call a = %v", calls[0])
	}
	if calls[1]["id"] != "call_b" || calls[1]["args"] != `{"x":2}` || calls[1]["done"] != `{"x":2}` {
		t.Errorf("call b = %v", calls[1])
	}
}

func TestEncodeStreamUnresolvedDeltaDroppedCleanly(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallDelta, Index: 7, ArgsFrag: `{"orphan":`},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	})
	frames := parseResponsesStream(t, body)
	assertResponsesWireOrder(t, frames)
	for _, f := range frames {
		if f.Name == "response.function_call_arguments.delta" {
			t.Fatalf("orphan function_call_arguments.delta leaked to wire: %v", f.Data)
		}
	}
}
