package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestEncodeResponseSkipsOpaqueOnlyResponsesReasoning(t *testing.T) {
	cipher := "cipher"
	resp := &ir.Response{ID: "msg_1", Model: "m", Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_1", Status: "completed", EncryptedContent: &cipher,
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: ""}},
		}},
		{Type: ir.BlockReasoning, Text: "readable", ResponsesReasoning: &ir.ResponsesReasoning{
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "readable"}},
		}},
		{Type: ir.BlockText, Text: "answer"},
	}}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Content []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			Text      string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Content) != 2 {
		t.Fatalf("content = %+v", wire.Content)
	}
	if wire.Content[0].Type != "thinking" || wire.Content[0].Thinking != "readable" || wire.Content[0].Signature != "" {
		t.Fatalf("readable reasoning = %+v", wire.Content[0])
	}
	if wire.Content[1].Type != "text" || wire.Content[1].Text != "answer" {
		t.Fatalf("text = %+v", wire.Content[1])
	}
}

func TestEncodeRequestSkipsOpaqueOnlyResponsesReasoning(t *testing.T) {
	cipher := ""
	req := &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_1", EncryptedContent: &cipher, Summary: []ir.ReasoningSummaryPart{},
		}},
		{Type: ir.BlockReasoning, Text: "readable", ResponsesReasoning: &ir.ResponsesReasoning{
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "readable"}},
		}},
		{Type: ir.BlockText, Text: "answer"},
	}}}}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 1 || len(wire.Messages[0].Content) != 2 {
		t.Fatalf("content = %+v", wire.Messages)
	}
	if wire.Messages[0].Content[0].Type != "thinking" || wire.Messages[0].Content[0].Thinking != "readable" {
		t.Fatalf("readable reasoning = %+v", wire.Messages[0].Content[0])
	}
	if wire.Messages[0].Content[1].Type != "text" {
		t.Fatalf("text = %+v", wire.Messages[0].Content[1])
	}
}

func TestEncodeResponseKeepsAnthropicSignatureOnlyBlock(t *testing.T) {
	resp := &ir.Response{ID: "msg_1", Model: "m", Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "", AnthropicSignature: "sig-only"},
	}}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"thinking":""`) || !strings.Contains(string(out), `"signature":"sig-only"`) {
		t.Fatalf("signature-only block lost: %s", out)
	}
}
