package openai

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// encodeStreamEvents drives an encoder over IR events and returns the wire.
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

// reassembleOpenAIToolCalls folds wire chunks back into per-client-index calls,
// keyed by the tool_calls index so split-identity bugs surface as missing or
// duplicated entries.
func reassembleOpenAIToolCalls(t *testing.T, body string) map[int]map[string]string {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	calls := map[int]map[string]string{}
	for {
		ev, err := reader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if string(ev.Data) == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(ev.Data, &chunk)
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				call, ok := calls[tc.Index]
				if !ok {
					call = map[string]string{}
					calls[tc.Index] = call
				}
				if tc.ID != "" {
					call["id"] = tc.ID
				}
				if tc.Function.Name != "" {
					call["name"] = tc.Function.Name
				}
				call["args"] += tc.Function.Arguments
			}
		}
	}
	return calls
}

func assertSingleToolCall(t *testing.T, calls map[int]map[string]string) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("want exactly one client tool call, got %d: %v", len(calls), calls)
	}
	var call map[string]string
	for _, c := range calls {
		call = c
	}
	if call["id"] != "call_1" {
		t.Errorf("tool id = %q, want call_1", call["id"])
	}
	if call["name"] != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", call["name"])
	}
	if call["args"] != `{"city":"SF"}` {
		t.Errorf("tool args = %q, want %q", call["args"], `{"city":"SF"}`)
	}
}

// Backends may stream argument fragments before the chunk carrying tool
// identity. The encoder must keep one client tool_calls index per IR index so
// the client reassembles a single valid call.
func TestEncodeStreamDeltaBeforeStartKeepsArgs(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "chatcmpl-1", Model: "m"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `{"ci`},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "call_1", ToolName: "get_weather"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `ty":"SF"}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	assertSingleToolCall(t, reassembleOpenAIToolCalls(t, body))
}

// Identity split across repeated Starts (id on one chunk, name on a later one)
// must reuse the same client index instead of allocating a second call.
func TestEncodeStreamSplitIdentityAcrossStartsKeepsArgs(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "chatcmpl-1", Model: "m"},
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1"},
		{Kind: ir.EventToolCallStart, Index: 0, ToolName: "get_weather"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"city":"SF"}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	assertSingleToolCall(t, reassembleOpenAIToolCalls(t, body))
}

// Interleaved parallel calls: each IR index keeps its own client index and
// fragments stay attributed to the right call.
func TestEncodeStreamInterleavedParallelCalls(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "chatcmpl-1", Model: "m"},
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_a", ToolName: "alpha"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `{"x":`},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "call_b", ToolName: "beta"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"a":1}`},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `2}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	calls := reassembleOpenAIToolCalls(t, body)
	if len(calls) != 2 {
		t.Fatalf("want two client tool calls, got %d: %v", len(calls), calls)
	}
	if calls[0]["id"] != "call_a" || calls[0]["name"] != "alpha" || calls[0]["args"] != `{"a":1}` {
		t.Errorf("call a = %v", calls[0])
	}
	if calls[1]["id"] != "call_b" || calls[1]["name"] != "beta" || calls[1]["args"] != `{"x":2}` {
		t.Errorf("call b = %v", calls[1])
	}
}
