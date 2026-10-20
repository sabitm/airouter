package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
	"net/http/httptest"
)

func TestServerBlockRoundTripKeepsFields(t *testing.T) {
	body := []byte(`{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://example.test","title":"Example"}]},{"type":"mcp_tool_use","id":"x","name":"t","server_name":"srv","input":{"a":1}}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if json.Unmarshal(out, &got) != nil || json.Unmarshal(body, &want) != nil {
		t.Fatal("json")
	}
	gb, _ := json.Marshal(got["content"])
	wb, _ := json.Marshal(want["content"])
	if string(gb) != string(wb) {
		t.Fatalf("content = %s\nwant %s", gb, wb)
	}
}

func TestServerStreamKeepsInputDelta(t *testing.T) {
	raw := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"web_search\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"x\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	enc := httptest.NewRecorder()
	w, ok := sse.NewWriter(enc)
	if !ok {
		t.Fatal("no writer")
	}
	e := NewStreamEncoder("x")
	err := DecodeStream(strings.NewReader(raw), func(ev ir.StreamEvent) error { return e.Encode(ev, w) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enc.Body.String(), `input_json_delta`) || strings.Contains(enc.Body.String(), `"type":"tool_use"`) {
		t.Fatalf("stream = %s", enc.Body.String())
	}
}
