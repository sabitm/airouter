package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestThinkingRequestRoundTripPreservesOpaqueBlocks(t *testing.T) {
	sig := "sig-exact-\u0001value"
	data := "redacted-exact-data"
	body := map[string]any{
		"model":      "m",
		"max_tokens": 128,
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "chain", "signature": sig},
				map[string]any{"type": "thinking", "thinking": "", "signature": "sig-empty"},
				map[string]any{"type": "thinking", "thinking": ""},
				map[string]any{"type": "redacted_thinking", "data": data},
				map[string]any{"type": "thinking", "thinking": "next", "signature": "sig-next"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "search", "input": map[string]any{"q": "x"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
			}},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := DecodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := req.Messages[0].Content
	if len(got) != 6 {
		t.Fatalf("blocks = %+v", got)
	}
	want := []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "chain", AnthropicSignature: sig},
		{Type: ir.BlockReasoning, Text: "", AnthropicSignature: "sig-empty"},
		{Type: ir.BlockReasoning, Text: ""},
		{Type: ir.BlockRedactedReasoning, RedactedData: data},
		{Type: ir.BlockReasoning, Text: "next", AnthropicSignature: "sig-next"},
		{Type: ir.BlockToolUse, ToolID: "toolu_1", ToolName: "search", ToolInput: json.RawMessage(`{"q":"x"}`)},
	}
	for i := range want {
		if got[i].Type != want[i].Type || got[i].Text != want[i].Text || got[i].AnthropicSignature != want[i].AnthropicSignature || got[i].RedactedData != want[i].RedactedData {
			t.Fatalf("block %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got[5].ToolName != "search" {
		t.Fatalf("tool = %+v", got[5])
	}

	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var encoded messagesRequest
	if err := json.Unmarshal(out, &encoded); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(encoded.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 6 {
		t.Fatalf("encoded blocks = %#v body=%s", blocks, out)
	}
	assertExact(t, blocks[0], "thinking", "chain", sig, "")
	assertExact(t, blocks[1], "thinking", "", "sig-empty", "")
	assertExact(t, blocks[2], "thinking", "", "", "")
	if blocks[3]["type"] != "redacted_thinking" || blocks[3]["data"] != data {
		t.Fatalf("redacted = %#v", blocks[3])
	}
	if _, ok := blocks[3]["thinking"]; ok {
		t.Fatalf("redacted block has thinking field: %#v", blocks[3])
	}
	assertExact(t, blocks[4], "thinking", "next", "sig-next", "")
	if blocks[5]["type"] != "tool_use" || blocks[5]["thinking"] != nil || blocks[5]["signature"] != nil || blocks[5]["data"] != nil {
		t.Fatalf("tool block gained thinking fields: %#v", blocks[5])
	}
	if !strings.Contains(string(out), `"thinking":""`) {
		t.Fatalf("empty thinking text omitted: %s", out)
	}
}

func TestThinkingResponseRoundTripPreservesOpaqueBlocks(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"chain","signature":"sig-a"},{"type":"thinking","thinking":"","signature":"sig-empty"},{"type":"redacted_thinking","data":"redacted-data"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 4 {
		t.Fatalf("content = %+v", resp.Content)
	}
	if resp.Content[1].Type != ir.BlockReasoning || resp.Content[1].Text != "" || resp.Content[1].AnthropicSignature != "sig-empty" {
		t.Fatalf("signature-only = %+v", resp.Content[1])
	}
	if resp.Content[2].Type != ir.BlockRedactedReasoning || resp.Content[2].RedactedData != "redacted-data" || resp.Content[2].Text != "" {
		t.Fatalf("redacted = %+v", resp.Content[2])
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]any
	if err := json.Unmarshal(out, &encoded); err != nil {
		t.Fatal(err)
	}
	content, _ := encoded["content"].([]any)
	if len(content) != 4 {
		t.Fatalf("content = %#v body=%s", content, out)
	}
	assertExact(t, content[0].(map[string]any), "thinking", "chain", "sig-a", "")
	assertExact(t, content[1].(map[string]any), "thinking", "", "sig-empty", "")
	red := content[2].(map[string]any)
	if red["type"] != "redacted_thinking" || red["data"] != "redacted-data" {
		t.Fatalf("redacted = %#v", red)
	}
	text := content[3].(map[string]any)
	if text["type"] != "text" || text["thinking"] != nil || text["signature"] != nil {
		t.Fatalf("text gained thinking fields: %#v", text)
	}
}

func assertExact(t *testing.T, block map[string]any, typ, thinking, signature, data string) {
	t.Helper()
	if block["type"] != typ || block["thinking"] != thinking || stringValue(block["signature"]) != signature || stringValue(block["data"]) != data {
		t.Fatalf("block = %#v, want type %s thinking %q signature %q data %q", block, typ, thinking, signature, data)
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
