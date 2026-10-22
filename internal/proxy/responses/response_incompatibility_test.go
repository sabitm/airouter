package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestEncodeResponseRejectsServerBlocks(t *testing.T) {
	cases := []struct {
		name    string
		content []ir.ContentBlock
	}{
		{
			name: "server-only",
			content: []ir.ContentBlock{{
				Type: ir.BlockServer,
				AnthropicServer: &ir.AnthropicServerBlock{
					WireType: "compaction",
					Raw:      json.RawMessage(`{"type":"compaction","content":"server-secret"}`),
				},
			}},
		},
		{
			name: "mixed-text-and-server",
			content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "primary-text"},
				{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{
					WireType: "compaction",
					Raw:      json.RawMessage(`{"type":"compaction","content":"server-secret"}`),
				}},
			},
		},
		{
			name:    "nil-server-metadata",
			content: []ir.ContentBlock{{Type: ir.BlockServer}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EncodeResponse(&ir.Response{Model: "m", Content: tc.content})
			if err == nil {
				t.Fatalf("server block became a success: %s", out)
			}
			if _, ok := ir.AsResponseIncompatibility(err); !ok {
				t.Fatalf("error type = %T, want response incompatibility", err)
			}
			if strings.Contains(err.Error(), "primary-text") || strings.Contains(err.Error(), "server-secret") || strings.Contains(err.Error(), "compaction") {
				t.Fatalf("error leaked response detail: %v", err)
			}
		})
	}
}

func TestEncodeResponseTextAndToolsUnchanged(t *testing.T) {
	resp := &ir.Response{
		Model: "m",
		Content: []ir.ContentBlock{
			{Type: ir.BlockReasoning, Text: "plan"},
			{Type: ir.BlockText, Text: "answer"},
			{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "lookup", ToolInput: json.RawMessage(`{"q":"x"}`)},
		},
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Output) != 3 || got.Output[0].Type != "reasoning" || got.Output[1].Type != "message" || got.Output[2].Type != "function_call" {
		t.Fatalf("output = %+v", got.Output)
	}
	if len(got.Output[1].Content) != 1 || got.Output[1].Content[0].Text != "answer" {
		t.Fatalf("message = %+v", got.Output[1])
	}
	if got.Output[2].CallID != "call_1" || got.Output[2].Name != "lookup" || got.Output[2].Arguments != `{"q":"x"}` {
		t.Fatalf("function call = %+v", got.Output[2])
	}
}
