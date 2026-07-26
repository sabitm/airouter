package claudecode

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeResponseDecloaksToolNames(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant","model":"up",
		"content":[
			{"type":"text","text":"hi"},
			{"type":"tool_use","id":"t1","name":"search_ide","input":{}},
			{"type":"tool_use","id":"t2","name":"Bash","input":{}}
		],
		"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}
	}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolUse {
			names = append(names, b.ToolName)
		}
	}
	if len(names) != 2 || names[0] != "search" || names[1] != "Bash" {
		t.Errorf("decloak mismatch: %v", names)
	}
}

func TestDecodeStreamDecloaksToolNames(t *testing.T) {
	const sse = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"search_ide","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var toolName string
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			toolName = ev.ToolName
		}
	}
	if toolName != "search" {
		t.Errorf("stream tool name not decloaked: %q (want search)", toolName)
	}
}
