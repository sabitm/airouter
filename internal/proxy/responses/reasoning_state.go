package responses

import (
	"bytes"
	"encoding/json"
	"fmt"

	"airouter/internal/proxy/ir"
)

// reasoningText is the readable projection of ordered summary parts. Opaque
// item state is never part of the projection.
func reasoningText(parts []ir.ReasoningSummaryPart) string {
	return (&ir.ResponsesReasoning{Summary: parts}).ReadableText()
}

// reasoningSummaryParts converts decode-side parts to canonical parts. Type and
// Text are preserved exactly; empty parts stay in the ordered list.
func reasoningSummaryParts(in []reasoningSummary) []ir.ReasoningSummaryPart {
	if in == nil {
		return nil
	}
	out := make([]ir.ReasoningSummaryPart, 0, len(in))
	for _, p := range in {
		out = append(out, ir.ReasoningSummaryPart{Type: p.Type, Text: p.Text})
	}
	return out
}

// reasoningBlock builds one canonical reasoning block from a Responses item.
// Exactly one block is produced per item, even when the readable projection is
// empty, so opaque-only items stay in order.
func reasoningBlock(id string, summary []ir.ReasoningSummaryPart, encrypted *string, status string) ir.ContentBlock {
	return ir.ContentBlock{
		Type: ir.BlockReasoning,
		Text: reasoningText(summary),
		ResponsesReasoning: &ir.ResponsesReasoning{
			ID:               id,
			Summary:          summary,
			EncryptedContent: encrypted,
			Status:           status,
		},
	}
}

// cloneResponsesReasoning deep-copies provider-owned state so a retained copy
// cannot be mutated through the source pointer.
func cloneResponsesReasoning(in *ir.ResponsesReasoning) *ir.ResponsesReasoning {
	return in.Clone()
}

// sameResponsesReasoning reports structural equality including explicit empty
// encrypted content versus absent. It is used to dedup repeat stream snapshots.
func sameResponsesReasoning(a, b *ir.ResponsesReasoning) bool {
	return a.Equal(b)
}

// reasoningOutputItem renders one reasoning item map. ID is preserved when the
// source supplied one. Output items generate a required wire ID only when the
// source omitted it; request input items keep an absent ID absent. Encrypted
// content is emitted only when the source supplied it, including an explicit
// empty string. Readable summary parts keep their order and boundaries.
func reasoningOutputItem(rr *ir.ResponsesReasoning, fallbackText string, generateID bool) map[string]any {
	id := ""
	status := ""
	encrypted := (*string)(nil)
	parts := []ir.ReasoningSummaryPart(nil)
	if rr != nil {
		id = rr.ID
		status = rr.Status
		encrypted = rr.EncryptedContent
		parts = rr.Summary
	}
	if generateID && id == "" {
		id = ir.NewID("rs_")
	}
	if len(parts) == 0 && fallbackText != "" {
		parts = []ir.ReasoningSummaryPart{{Type: "summary_text", Text: fallbackText}}
	}
	summary := make([]any, 0, len(parts))
	for _, p := range parts {
		typ := p.Type
		if typ == "" {
			typ = "summary_text"
		}
		summary = append(summary, map[string]any{"type": typ, "text": p.Text})
	}
	item := map[string]any{"type": "reasoning", "summary": summary}
	if id != "" {
		item["id"] = id
	}
	if encrypted != nil {
		item["encrypted_content"] = *encrypted
	}
	if status != "" {
		item["status"] = status
	}
	return item
}

// decodeReasoningInputItem strictly decodes the supported fields of one
// Responses reasoning input item. Unsupported extra keys are ignored, but a
// malformed supported field is an error so opaque state cannot be silently
// dropped or reinterpreted. Errors name only the field or index.
func decodeReasoningInputItem(raw json.RawMessage) (ir.ContentBlock, error) {
	var wire struct {
		ID               json.RawMessage `json:"id"`
		Summary          json.RawMessage `json:"summary"`
		EncryptedContent json.RawMessage `json:"encrypted_content"`
		Status           json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ir.ContentBlock{}, fmt.Errorf("responses: decode request: reasoning item is not an object")
	}
	id, err := optionalWireString(wire.ID, "reasoning item id")
	if err != nil {
		return ir.ContentBlock{}, err
	}
	status, err := optionalWireString(wire.Status, "reasoning item status")
	if err != nil {
		return ir.ContentBlock{}, err
	}
	encrypted, err := optionalWireStringPtr(wire.EncryptedContent, "reasoning item encrypted_content")
	if err != nil {
		return ir.ContentBlock{}, err
	}
	summary, err := decodeReasoningSummary(wire.Summary)
	if err != nil {
		return ir.ContentBlock{}, err
	}
	return reasoningBlock(id, summary, encrypted, status), nil
}

func decodeReasoningSummary(raw json.RawMessage) ([]ir.ReasoningSummaryPart, error) {
	if isNullish(raw) {
		return nil, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, fmt.Errorf("responses: decode request: reasoning item summary must be an array")
	}
	out := make([]ir.ReasoningSummaryPart, 0, len(raws))
	for i, p := range raws {
		// A JSON null array element is not an empty part. Accepting it would
		// reinterpret malformed supported state as an absent field.
		if isNullish(p) {
			return nil, fmt.Errorf("responses: decode request: reasoning item summary[%d] must be an object", i)
		}
		var part struct {
			Type json.RawMessage `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if err := json.Unmarshal(p, &part); err != nil {
			return nil, fmt.Errorf("responses: decode request: reasoning item summary[%d] must be an object", i)
		}
		typ, err := optionalWireString(part.Type, fmt.Sprintf("reasoning item summary[%d] type", i))
		if err != nil {
			return nil, err
		}
		text, err := optionalWireString(part.Text, fmt.Sprintf("reasoning item summary[%d] text", i))
		if err != nil {
			return nil, err
		}
		out = append(out, ir.ReasoningSummaryPart{Type: typ, Text: text})
	}
	return out, nil
}

// optionalWireString accepts an absent or null field as empty. A present value
// must be a JSON string; numbers, booleans, arrays, and objects are rejected.
func optionalWireString(raw json.RawMessage, what string) (string, error) {
	if isNullish(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("responses: decode request: %s must be a string", what)
	}
	return s, nil
}

// optionalWireStringPtr distinguishes absent/null (nil) from an explicit string
// including the empty string.
func optionalWireStringPtr(raw json.RawMessage, what string) (*string, error) {
	if isNullish(raw) {
		return nil, nil
	}
	s, err := optionalWireString(raw, what)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func isNullish(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
