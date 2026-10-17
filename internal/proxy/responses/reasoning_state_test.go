package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

const structuredReasoningItem = `{"type":"reasoning","id":"rs_1","status":"completed","encrypted_content":"cipher","summary":[{"type":"summary_text","text":"part-a"},{"type":"summary_text","text":""},{"type":"other","text":"part-b"}]}`

func TestDecodeRequestReasoningStateStructured(t *testing.T) {
	body := `{"model":"m","input":[` + structuredReasoningItem + `,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if len(req.Messages[0].Content) != 2 {
		t.Fatalf("assistant content = %+v", req.Messages[0].Content)
	}
	block := req.Messages[0].Content[0]
	if block.Type != ir.BlockReasoning || block.Text != "part-apart-b" {
		t.Fatalf("block = %+v", block)
	}
	if block.AnthropicSignature != "" || block.RedactedData != "" {
		t.Fatalf("opaque bytes leaked into other fields: %+v", block)
	}
	rr := block.ResponsesReasoning
	if rr == nil {
		t.Fatal("missing structured reasoning envelope")
	}
	if rr.ID != "rs_1" || rr.Status != "completed" {
		t.Fatalf("identity = %+v", rr)
	}
	if rr.EncryptedContent == nil || *rr.EncryptedContent != "cipher" {
		t.Fatalf("encrypted = %+v", rr.EncryptedContent)
	}
	want := []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "part-a"}, {Type: "summary_text", Text: ""}, {Type: "other", Text: "part-b"}}
	if len(rr.Summary) != len(want) {
		t.Fatalf("summary = %+v", rr.Summary)
	}
	for i := range want {
		if rr.Summary[i] != want[i] {
			t.Fatalf("summary[%d] = %+v want %+v", i, rr.Summary[i], want[i])
		}
	}
}

func TestDecodeRequestReasoningStateOpaqueOnly(t *testing.T) {
	body := `{"model":"m","input":[{"type":"reasoning","id":"rs_opaque","encrypted_content":"","summary":[]},{"type":"message","role":"user","content":"next"}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 || len(req.Messages[0].Content) != 1 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	block := req.Messages[0].Content[0]
	if block.Text != "" || block.ResponsesReasoning == nil {
		t.Fatalf("block = %+v", block)
	}
	if block.ResponsesReasoning.EncryptedContent == nil || *block.ResponsesReasoning.EncryptedContent != "" {
		t.Fatalf("empty encrypted content lost: %+v", block.ResponsesReasoning.EncryptedContent)
	}
}

func TestDecodeRequestReasoningStateEncryptedAbsentNullEmpty(t *testing.T) {
	cases := []struct {
		name string
		item string
		want *string
	}{
		{"absent", `{"type":"reasoning"}`, nil},
		{"null", `{"type":"reasoning","encrypted_content":null}`, nil},
		{"empty", `{"type":"reasoning","encrypted_content":""}`, strPtr("")},
		{"value", `{"type":"reasoning","encrypted_content":"abc"}`, strPtr("abc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := DecodeRequest([]byte(`{"model":"m","input":[` + tc.item + `]}`))
			if err != nil {
				t.Fatal(err)
			}
			got := req.Messages[0].Content[0].ResponsesReasoning.EncryptedContent
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("encrypted = %v want %v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("encrypted = %q want %q", *got, *tc.want)
			}
		})
	}
}

func TestDecodeRequestReasoningStateMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"encrypted number", `{"type":"reasoning","encrypted_content":123}`},
		{"encrypted object", `{"type":"reasoning","encrypted_content":{"a":1}}`},
		{"encrypted array", `{"type":"reasoning","encrypted_content":[]}`},
		{"encrypted bool", `{"type":"reasoning","encrypted_content":true}`},
		{"id number", `{"type":"reasoning","id":7}`},
		{"status number", `{"type":"reasoning","status":7}`},
		{"summary object", `{"type":"reasoning","summary":{"type":"summary_text"}}`},
		{"summary element string", `{"type":"reasoning","summary":["text"]}`},
		{"summary text number", `{"type":"reasoning","summary":[{"type":"summary_text","text":7}]}`},
		{"summary type number", `{"type":"reasoning","summary":[{"type":7,"text":"x"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(`{"model":"m","input":[` + tc.raw + `]}`))
			if err == nil {
				t.Fatal("want decode error")
			}
			if !strings.Contains(err.Error(), "reasoning item") {
				t.Fatalf("error lost field context: %v", err)
			}
			if strings.Contains(err.Error(), "123") || strings.Contains(err.Error(), `"a"`) {
				t.Fatalf("error echoed payload: %v", err)
			}
		})
	}
}

func TestEncodeRequestReasoningStateRoundTrip(t *testing.T) {
	body := `{"model":"m","input":[` + structuredReasoningItem + `,{"type":"message","role":"user","content":"next"}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 2 {
		t.Fatalf("input = %d items: %s", len(wire.Input), out)
	}
	var item struct {
		Type             string  `json:"type"`
		ID               string  `json:"id"`
		Status           string  `json:"status"`
		EncryptedContent *string `json:"encrypted_content"`
		Summary          []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(wire.Input[0], &item); err != nil {
		t.Fatal(err)
	}
	if item.Type != "reasoning" || item.ID != "rs_1" || item.Status != "completed" {
		t.Fatalf("item = %+v", item)
	}
	if item.EncryptedContent == nil || *item.EncryptedContent != "cipher" {
		t.Fatalf("encrypted = %+v", item.EncryptedContent)
	}
	if len(item.Summary) != 3 || item.Summary[1].Text != "" || item.Summary[2].Type != "other" {
		t.Fatalf("summary = %+v", item.Summary)
	}
	if wire.Input[1] == nil {
		t.Fatal("user message lost")
	}
}

func TestEncodeRequestReasoningKeepsAbsentIDsAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "legacy"},
		{Type: ir.BlockReasoning, Text: "structured", ResponsesReasoning: &ir.ResponsesReasoning{
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "structured"}},
		}},
		{Type: ir.BlockReasoning, Text: "supplied", ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_supplied", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "supplied"}},
		}},
	}}}}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 3 {
		t.Fatalf("input = %+v", wire.Input)
	}
	if _, ok := wire.Input[0]["id"]; ok {
		t.Fatalf("plain summary invented a request id: %s", out)
	}
	if _, ok := wire.Input[1]["id"]; ok {
		t.Fatalf("structured summary invented a request id: %s", out)
	}
	var id string
	if err := json.Unmarshal(wire.Input[2]["id"], &id); err != nil || id != "rs_supplied" {
		t.Fatalf("supplied id = %q err=%v (%s)", id, err, out)
	}
}

func TestEncodeRequestReasoningPreservesExactOpaqueFields(t *testing.T) {
	enc := "cipher\u0000-utf8-\u00e9"
	empty := ""
	req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "a", ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_a", Status: "completed", EncryptedContent: &enc,
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}},
		}},
		{Type: ir.BlockReasoning, ResponsesReasoning: &ir.ResponsesReasoning{ID: "rs_b", EncryptedContent: &empty}},
		{Type: ir.BlockReasoning, ResponsesReasoning: &ir.ResponsesReasoning{ID: "rs_c"}},
	}}}}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 3 {
		t.Fatalf("input = %+v", wire.Input)
	}
	var gotEnc string
	if err := json.Unmarshal(wire.Input[0]["encrypted_content"], &gotEnc); err != nil || gotEnc != enc {
		t.Fatalf("encrypted = %q err=%v (%s)", gotEnc, err, out)
	}
	var gotStatus string
	if err := json.Unmarshal(wire.Input[0]["status"], &gotStatus); err != nil || gotStatus != "completed" {
		t.Fatalf("status = %q err=%v", gotStatus, err)
	}
	if string(wire.Input[1]["encrypted_content"]) != `""` {
		t.Fatalf("explicit empty encrypted lost: %s", out)
	}
	if _, ok := wire.Input[2]["encrypted_content"]; ok {
		t.Fatalf("absent encrypted invented: %s", out)
	}
	if _, ok := wire.Input[2]["status"]; ok {
		t.Fatalf("absent status invented: %s", out)
	}
}

func TestDecodeRequestReasoningSummaryNullPartRejected(t *testing.T) {
	for _, raw := range []string{
		`{"type":"reasoning","summary":[null]}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"a"},null]}`,
	} {
		_, err := DecodeRequest([]byte(`{"model":"m","input":[` + raw + `]}`))
		if err == nil {
			t.Fatalf("null summary part accepted: %s", raw)
		}
		if !strings.Contains(err.Error(), "summary") {
			t.Fatalf("error lost field context: %v", err)
		}
		if strings.Contains(err.Error(), "null") || strings.Contains(err.Error(), "summary_text") {
			t.Fatalf("error echoed payload: %v", err)
		}
	}
	// The whole summary field may be null; that is an absent field, not a null
	// array entry.
	req, err := DecodeRequest([]byte(`{"model":"m","input":[{"type":"reasoning","id":"rs_1","summary":null,"encrypted_content":"c"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rr := req.Messages[0].Content[0].ResponsesReasoning
	if rr == nil || rr.Summary != nil || rr.EncryptedContent == nil || *rr.EncryptedContent != "c" {
		t.Fatalf("null summary handling = %+v", rr)
	}
}

func TestDecodeRequestMalformedSiblingPreservesReasoningState(t *testing.T) {
	opaque := `{"type":"reasoning","id":"rs_keep","encrypted_content":"cipher-keep","summary":[{"type":"summary_text","text":"prior"}]}`
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"nonobject sibling after", `{"model":"m","input":[` + opaque + `,123]}`, false},
		{"invalid discriminator sibling", `{"model":"m","input":[` + opaque + `,{"type":{"nested":1}}]}`, false},
		{"nonobject sibling before", `{"model":"m","input":[123,` + opaque + `]}`, false},
		{"malformed reasoning sibling after", `{"model":"m","input":[` + opaque + `,{"type":"reasoning","encrypted_content":123}]}`, true},
		{"null summary sibling after", `{"model":"m","input":[` + opaque + `,{"type":"reasoning","summary":[null]}]}`, true},
		{"malformed reasoning sibling before", `{"model":"m","input":[{"type":"reasoning","id":7},` + opaque + `]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := DecodeRequest([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("malformed reasoning state accepted")
				}
				if strings.Contains(err.Error(), "cipher-keep") || strings.Contains(err.Error(), "123") {
					t.Fatalf("error echoed payload: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, msg := range req.Messages {
				for _, b := range msg.Content {
					if b.ResponsesReasoning != nil && b.ResponsesReasoning.ID == "rs_keep" &&
						b.ResponsesReasoning.EncryptedContent != nil && *b.ResponsesReasoning.EncryptedContent == "cipher-keep" {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("supported opaque history lost: %+v", req.Messages)
			}
		})
	}
}

func TestEncodeResponsePlainReasoningGeneratesOutputID(t *testing.T) {
	resp := &ir.Response{Model: "m", Content: []ir.ContentBlock{{Type: ir.BlockReasoning, Text: "readable"}}}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire respObject
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Output) != 1 || wire.Output[0].Type != "reasoning" || wire.Output[0].ID == "" {
		t.Fatalf("output reasoning id missing: %s", out)
	}
	if len(wire.Output[0].Summary) != 1 || wire.Output[0].Summary[0].Text != "readable" {
		t.Fatalf("summary = %+v", wire.Output[0].Summary)
	}
}

func TestDecodeResponseReasoningStateStructured(t *testing.T) {
	body := `{"id":"resp_1","model":"m","status":"completed","output":[` + structuredReasoningItem + `,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %+v", resp.Content)
	}
	block := resp.Content[0]
	if block.Type != ir.BlockReasoning || block.Text != "part-apart-b" {
		t.Fatalf("block = %+v", block)
	}
	if block.AnthropicSignature != "" || block.RedactedData != "" {
		t.Fatalf("opaque leaked: %+v", block)
	}
	if block.ResponsesReasoning == nil || block.ResponsesReasoning.ID != "rs_1" {
		t.Fatalf("envelope = %+v", block.ResponsesReasoning)
	}
	if block.ResponsesReasoning.EncryptedContent == nil || *block.ResponsesReasoning.EncryptedContent != "cipher" {
		t.Fatalf("encrypted = %+v", block.ResponsesReasoning.EncryptedContent)
	}
	if resp.Content[1].Type != ir.BlockText || resp.Content[1].Text != "answer" {
		t.Fatalf("text = %+v", resp.Content[1])
	}
}

func TestDecodeResponseReasoningEmptySummaryKeepsBlock(t *testing.T) {
	body := `{"id":"resp_1","model":"m","status":"completed","output":[{"id":"rs_empty","type":"reasoning","summary":[]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockReasoning || resp.Content[0].ResponsesReasoning == nil {
		t.Fatalf("content = %+v", resp.Content)
	}
}

func TestEncodeResponseReasoningOrderAndState(t *testing.T) {
	cipher := "cipher"
	resp := &ir.Response{ID: "resp_1", Model: "m", Content: []ir.ContentBlock{
		{Type: ir.BlockText, Text: "first"},
		{Type: ir.BlockReasoning, Text: "chain", ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_1", Status: "completed", EncryptedContent: &cipher,
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}, {Type: "summary_text", Text: ""}},
		}},
		{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "fn", ToolInput: json.RawMessage(`{"q":"x"}`)},
		{Type: ir.BlockText, Text: "last"},
	}}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire respObject
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Output) != 4 {
		t.Fatalf("output = %+v", wire.Output)
	}
	wantTypes := []string{"message", "reasoning", "function_call", "message"}
	for i, want := range wantTypes {
		if wire.Output[i].Type != want {
			t.Fatalf("output[%d].type = %q want %q (%s)", i, wire.Output[i].Type, want, out)
		}
	}
	reasoning := wire.Output[1]
	if reasoning.ID != "rs_1" || reasoning.Status != "completed" {
		t.Fatalf("reasoning = %+v", reasoning)
	}
	if reasoning.EncryptedContent == nil || *reasoning.EncryptedContent != "cipher" {
		t.Fatalf("encrypted = %+v", reasoning.EncryptedContent)
	}
	if len(reasoning.Summary) != 2 || reasoning.Summary[1].Text != "" {
		t.Fatalf("summary = %+v", reasoning.Summary)
	}
	if wire.Output[0].Content[0].Text != "first" || wire.Output[3].Content[0].Text != "last" {
		t.Fatalf("text order lost: %+v", wire.Output)
	}
}

func TestEncodeCodexRequestPreservesResponsesReasoning(t *testing.T) {
	cipher := ""
	req := &ir.Request{Model: "gpt-5-codex", Stream: true, Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "chain", ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_9", Status: "completed", EncryptedContent: &cipher,
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
		}},
	}}}}
	out, err := EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []struct {
			Type             string  `json:"type"`
			ID               string  `json:"id"`
			Status           string  `json:"status"`
			EncryptedContent *string `json:"encrypted_content"`
			Summary          []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 1 {
		t.Fatalf("input = %+v", wire.Input)
	}
	item := wire.Input[0]
	if item.Type != "reasoning" || item.ID != "rs_9" || item.Status != "completed" {
		t.Fatalf("item = %+v", item)
	}
	if item.EncryptedContent == nil || *item.EncryptedContent != "" {
		t.Fatalf("empty encrypted content not preserved: %+v", item.EncryptedContent)
	}
	if len(item.Summary) != 1 || item.Summary[0].Text != "chain" {
		t.Fatalf("summary = %+v", item.Summary)
	}
}

func strPtr(s string) *string { return &s }
