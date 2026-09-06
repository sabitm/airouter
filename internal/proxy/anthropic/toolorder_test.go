package anthropic

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func encodeStreamEvents(t *testing.T, _ string, events []ir.StreamEvent) string {
	t.Helper()
	return encodeStream(t, events)
}

// wireFrame is one parsed Anthropic SSE event.
type wireFrame struct {
	Name string
	Data map[string]any
}

// parseAnthropicStream returns the ordered event names plus per-event payloads,
// so ordering assertions (message_start before any content_block_delta) are
// first-class.
func parseAnthropicStream(t *testing.T, body string) []wireFrame {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var frames []wireFrame
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
		frames = append(frames, wireFrame{Name: ev.Name, Data: data})
	}
	return frames
}

// reassembleAnthropicToolBlocks folds input_json_delta frames back into
// per-block-index calls keyed by the content_block_start identity.
func reassembleAnthropicToolBlocks(t *testing.T, frames []wireFrame) map[int]map[string]string {
	t.Helper()
	blocks := map[int]map[string]string{}
	for _, f := range frames {
		switch f.Name {
		case "content_block_start":
			idx := int(f.Data["index"].(float64))
			block, _ := f.Data["content_block"].(map[string]any)
			if block["type"] != "tool_use" {
				continue
			}
			call, ok := blocks[idx]
			if !ok {
				call = map[string]string{}
				blocks[idx] = call
			}
			if id, ok := block["id"].(string); ok && id != "" {
				call["id"] = id
			}
			if name, ok := block["name"].(string); ok && name != "" {
				call["name"] = name
			}
		case "content_block_delta":
			idx := int(f.Data["index"].(float64))
			delta, _ := f.Data["delta"].(map[string]any)
			if delta["type"] != "input_json_delta" {
				continue
			}
			call, ok := blocks[idx]
			if !ok {
				call = map[string]string{}
				blocks[idx] = call
			}
			call["args"] += delta["partial_json"].(string)
		}
	}
	return blocks
}

func assertAnthropicWireOrder(t *testing.T, frames []wireFrame) {
	t.Helper()
	sawMessageStart := false
	openBlocks := map[int]bool{}
	for i, f := range frames {
		switch f.Name {
		case "message_start":
			if sawMessageStart {
				t.Errorf("duplicate message_start at frame %d", i)
			}
			sawMessageStart = true
		case "content_block_start":
			idx := int(f.Data["index"].(float64))
			if !sawMessageStart {
				t.Errorf("content_block_start before message_start at frame %d", i)
			}
			if openBlocks[idx] {
				t.Errorf("duplicate open block %d at frame %d", idx, i)
			}
			openBlocks[idx] = true
		case "content_block_delta", "content_block_stop":
			idx := int(f.Data["index"].(float64))
			if !sawMessageStart {
				t.Fatalf("%s before message_start at frame %d", f.Name, i)
			}
			if !openBlocks[idx] {
				t.Fatalf("%s for unopened block %d at frame %d", f.Name, idx, i)
			}
			if f.Name == "content_block_stop" {
				delete(openBlocks, idx)
			}
		}
	}
}

func oneTool(t *testing.T, blocks map[int]map[string]string) map[string]string {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("want one tool block, got %d: %v", len(blocks), blocks)
	}
	for _, call := range blocks {
		return call
	}
	return nil
}

// Backends may stream argument fragments before the chunk carrying tool
// identity. The Anthropic wire requires message_start and an open
// content_block before any delta, so fragments must be buffered and flushed
// after the eventual content_block_start, never emitted out of band.
func TestEncodeStreamDeltaBeforeStartKeepsArgs(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `{"ci`},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "call_1", ToolName: "get_weather"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `ty":"SF"}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	frames := parseAnthropicStream(t, body)
	assertAnthropicWireOrder(t, frames)
	call := oneTool(t, reassembleAnthropicToolBlocks(t, frames))
	if call["id"] != "call_1" || call["name"] != "get_weather" {
		t.Errorf("identity = %v", call)
	}
	if call["args"] != `{"city":"SF"}` {
		t.Errorf("args = %q, want %q", call["args"], `{"city":"SF"}`)
	}
}

// Identity split across repeated Starts must reuse the existing block and
// backfill identity, not open a second block splitting the call.
func TestEncodeStreamSplitIdentityAcrossStartsKeepsArgs(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1"},
		{Kind: ir.EventToolCallStart, Index: 0, ToolName: "get_weather"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"city":"SF"}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	frames := parseAnthropicStream(t, body)
	assertAnthropicWireOrder(t, frames)
	call := oneTool(t, reassembleAnthropicToolBlocks(t, frames))
	if call["id"] != "call_1" || call["name"] != "get_weather" {
		t.Errorf("identity = %v", call)
	}
	if call["args"] != `{"city":"SF"}` {
		t.Errorf("args = %q, want %q", call["args"], `{"city":"SF"}`)
	}
}

// Interleaved parallel calls: a fragment for tool B arriving while tool A is
// open must stay attributed to B's block once B opens, never merge into A.
func TestEncodeStreamInterleavedParallelCalls(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_a", ToolName: "alpha"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `{"x":`},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "call_b", ToolName: "beta"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"a":1}`},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `2}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	frames := parseAnthropicStream(t, body)
	assertAnthropicWireOrder(t, frames)
	blocks := reassembleAnthropicToolBlocks(t, frames)
	if len(blocks) != 2 {
		t.Fatalf("want two tool blocks, got %d: %v", len(blocks), blocks)
	}
	if blocks[0]["id"] != "call_a" || blocks[0]["args"] != `{"a":1}` {
		t.Errorf("block a = %v", blocks[0])
	}
	if blocks[1]["id"] != "call_b" || blocks[1]["args"] != `{"x":2}` {
		t.Errorf("block b = %v", blocks[1])
	}
}

// A delta whose Start never arrives has no valid block to live on; the wire
// must stay protocol-valid (no orphan content_block_delta) and the stream must
// still terminate cleanly.
func TestEncodeStreamUnresolvedDeltaDroppedCleanly(t *testing.T) {
	body := encodeStreamEvents(t, "m", []ir.StreamEvent{
		{Kind: ir.EventToolCallDelta, Index: 7, ArgsFrag: `{"orphan":`},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	})
	frames := parseAnthropicStream(t, body)
	assertAnthropicWireOrder(t, frames)
	for _, f := range frames {
		if f.Name == "content_block_delta" {
			delta, _ := f.Data["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				t.Fatalf("orphan input_json_delta leaked to wire: %v", f.Data)
			}
		}
	}
}
