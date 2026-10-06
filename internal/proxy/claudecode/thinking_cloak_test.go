package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCloakPreservesSignedEmptyAndRedactedThinking(t *testing.T) {
	content, err := json.Marshal([]wireBlock{
		{Type: "thinking", Thinking: thinkingPtr("chain"), Signature: "sig-a"},
		{Type: "thinking", Thinking: thinkingPtr(""), Signature: "sig-empty"},
		{Type: "redacted_thinking", Data: "opaque-data"},
		{Type: "tool_use", ID: "t1", Name: "search", Input: json.RawMessage(`{"q":"x"}`)},
		{Type: "text", Text: "answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	in, err := json.Marshal(messagesBody{
		Model:     "claude-x",
		MaxTokens: 16,
		Tools:     []wireTool{{Name: "search"}},
		Messages:  []wireMessage{{Role: "assistant", Content: content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	var blocks []wireBlock
	mustJSON(t, m.Messages[0].Content, &blocks)
	if len(blocks) != 5 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[3].Type != "tool_use" || blocks[3].Name != "search"+ToolSuffix {
		t.Fatalf("tool rename did not run: %+v", blocks[3])
	}
	if blocks[0].Type != "thinking" || thinkingText(blocks[0].Thinking) != "chain" || blocks[0].Signature != "sig-a" {
		t.Fatalf("signed thinking lost: %+v", blocks[0])
	}
	if blocks[1].Type != "thinking" || thinkingText(blocks[1].Thinking) != "" || blocks[1].Signature != "sig-empty" {
		t.Fatalf("empty signed thinking lost: %+v", blocks[1])
	}
	if blocks[2].Type != "redacted_thinking" || blocks[2].Data != "opaque-data" {
		t.Fatalf("redacted lost: %+v", blocks[2])
	}
	if blocks[4].Type != "text" || blocks[4].Text != "answer" || blocks[4].Thinking != nil || blocks[4].Signature != "" {
		t.Fatalf("text changed: %+v", blocks[4])
	}
	raw := string(m.Messages[0].Content)
	if !strings.Contains(raw, `"thinking":""`) || !strings.Contains(raw, `"signature":"sig-empty"`) {
		t.Fatalf("empty thinking omitted: %s", raw)
	}
}

func thinkingPtr(s string) *string { return &s }

func thinkingText(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
