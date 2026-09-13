package anthropic

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestDecodeResponseInclusivePlusCache(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":200,"cache_read_input_tokens":2000,"cache_creation_input_tokens":400,"output_tokens":300}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 2600 || resp.Usage.OutputTokens != 300 {
		t.Fatalf("totals = %+v", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 2000 || resp.Usage.CacheWriteTokens != 400 {
		t.Fatalf("cache = %+v", resp.Usage)
	}
}

func TestEncodeResponsePartitionsWithoutDoubleCount(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "msg_1", Model: "m",
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		Usage:   ir.Usage{InputTokens: 2600, OutputTokens: 300, CacheReadTokens: 2000, CacheWriteTokens: 400},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got messagesResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.InputTokens != 200 || got.Usage.CacheReadInputTokens != 2000 || got.Usage.CacheCreationInputTokens != 400 {
		t.Fatalf("partition = %+v body=%s", got.Usage, out)
	}
	if got.Usage.OutputTokens != 300 {
		t.Fatalf("output = %d", got.Usage.OutputTokens)
	}
	if got.Usage.TotalInput() != 2600 {
		t.Fatalf("total input = %d", got.Usage.TotalInput())
	}
}

func TestEncodeResponseZeroDetailsUnchanged(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "msg_1", Model: "m",
		Usage: ir.Usage{InputTokens: 13, OutputTokens: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got messagesResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.InputTokens != 13 || got.Usage.CacheReadInputTokens != 0 || got.Usage.CacheCreationInputTokens != 0 {
		t.Fatalf("zero-details mutated: %+v body=%s", got.Usage, out)
	}
}

func TestEncodeResponseClampsOverlargeCache(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "msg_1", Model: "m",
		Usage: ir.Usage{InputTokens: 10, OutputTokens: 1, CacheReadTokens: 9, CacheWriteTokens: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got messagesResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.InputTokens != 0 || got.Usage.CacheReadInputTokens != 9 || got.Usage.CacheCreationInputTokens != 1 {
		t.Fatalf("clamped partition = %+v", got.Usage)
	}
	if got.Usage.InputTokens < 0 {
		t.Fatal("ordinary input negative")
	}
}

func TestDecodeStreamInclusivePlusCache(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":200,"cache_read_input_tokens":2000,"cache_creation_input_tokens":400,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Kind != ir.EventMessageStart {
		t.Fatalf("events = %+v", events)
	}
	if events[0].InputTokens != 2600 || events[0].CacheReadTokens != 2000 || events[0].CacheWriteTokens != 400 {
		t.Fatalf("start = %+v", events[0])
	}
	finish := events[len(events)-1]
	if finish.Kind != ir.EventFinish || finish.InputTokens != 2600 || finish.CacheReadTokens != 2000 || finish.CacheWriteTokens != 400 {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestEncodeStreamPartitionsCacheOnStart(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m", InputTokens: 2600, CacheReadTokens: 2000, CacheWriteTokens: 400},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 2600, OutputTokens: 3, CacheReadTokens: 2000, CacheWriteTokens: 400},
	})
	startIn, deltaIn, deltaOut := collectAnthropicUsageFrames(t, body)
	if startIn != 200 {
		t.Fatalf("message_start ordinary input = %d, want 200; body=%s", startIn, body)
	}
	if deltaIn != nil {
		t.Fatalf("message_delta input_tokens = %v, want omitted", deltaIn)
	}
	if deltaOut != 3 {
		t.Fatalf("output = %d", deltaOut)
	}
	if !strings.Contains(body, `"cache_read_input_tokens":2000`) || !strings.Contains(body, `"cache_creation_input_tokens":400`) {
		t.Fatalf("start cache missing: %s", body)
	}
}

func TestEncodeStreamZeroDetailsUnchanged(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m", InputTokens: 12},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 12, OutputTokens: 3},
	})
	startIn, deltaIn, _ := collectAnthropicUsageFrames(t, body)
	if startIn != 12 {
		t.Fatalf("start = %d", startIn)
	}
	if deltaIn != nil {
		t.Fatalf("delta input = %v", deltaIn)
	}
	if strings.Contains(body, "cache_read_input_tokens") || strings.Contains(body, "cache_creation_input_tokens") {
		t.Fatalf("zero cache emitted: %s", body)
	}
}

func TestEncodeStreamLateCacheCorrectsOrdinaryInput(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m", InputTokens: 2600},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 2600, OutputTokens: 300, CacheReadTokens: 2000, CacheWriteTokens: 400},
	})
	start, delta := parseAnthropicUsageEvents(t, body)
	if start.InputTokens != 2600 {
		t.Fatalf("message_start input_tokens = %d, want 2600", start.InputTokens)
	}
	if start.CacheReadInputTokens != 0 || start.CacheCreationInputTokens != 0 {
		t.Fatalf("message_start cache = %+v", start)
	}
	if delta.InputTokens == nil || *delta.InputTokens != 200 {
		t.Fatalf("message_delta input_tokens = %v, want 200", ptrVal(delta.InputTokens))
	}
	if delta.CacheReadInputTokens == nil || *delta.CacheReadInputTokens != 2000 {
		t.Fatalf("message_delta cache_read_input_tokens = %v, want 2000", ptrVal(delta.CacheReadInputTokens))
	}
	if delta.CacheCreationInputTokens == nil || *delta.CacheCreationInputTokens != 400 {
		t.Fatalf("message_delta cache_creation_input_tokens = %v, want 400", ptrVal(delta.CacheCreationInputTokens))
	}
	if delta.OutputTokens != 300 {
		t.Fatalf("message_delta output_tokens = %d, want 300", delta.OutputTokens)
	}
}

type anthDeltaUsage struct {
	InputTokens              *int `json:"input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
}

func parseAnthropicUsageEvents(t *testing.T, body string) (start anthUsage, delta anthDeltaUsage) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
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
					Usage anthUsage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				t.Fatalf("message_start: %s", ev.Data)
			}
			start = d.Message.Usage
		case "message_delta":
			var d struct {
				Usage anthDeltaUsage `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				t.Fatalf("message_delta: %s", ev.Data)
			}
			delta = d.Usage
		}
	}
	return start, delta
}

func ptrVal(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
