package anthropic

import (
	"encoding/json"
	"fmt"

	"airouter/internal/proxy/ir"
)

// DecodeResponse parses an Anthropic Messages response into the IR. Used when
// Anthropic is the backend format.
func DecodeResponse(body []byte) (*ir.Response, error) {
	var resp messagesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	state, err := validateResponseState(resp.Container, resp.ContextManagement, resp.StopDetails, resp.InputTransformations)
	if err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	out := &ir.Response{
		ID:           resp.ID,
		Model:        resp.Model,
		StopReason:   stopReason(resp.StopReason),
		StopSequence: resp.StopSequence,
		Usage:        usageFromAnth(resp.Usage),
		Anthropic:    state,
	}
	var rawContent []json.RawMessage
	_ = json.Unmarshal(mustContent(body), &rawContent)
	for i, b := range resp.Content {
		switch b.Type {
		case "thinking":
			out.Content = append(out.Content, ir.ContentBlock{
				Type: ir.BlockReasoning, Text: thinkingText(b.Thinking), AnthropicSignature: b.Signature,
			})
		case "redacted_thinking":
			out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockRedactedReasoning, RedactedData: b.Data})
		case "text":
			citations, err := decodeCitations(b.Citations)
			if err != nil {
				return nil, fmt.Errorf("anthropic: decode response: %w", err)
			}
			out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockText, Text: b.Text, Citations: citations})
		case "tool_use":
			out.Content = append(out.Content, ir.ContentBlock{
				Type:      ir.BlockToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				ToolInput: b.Input,
			})
		default:
			if b.Type != "" {
				if !knownResponseServerBlockType(b.Type) {
					return nil, fmt.Errorf("anthropic: decode response: unsupported server block type %q", b.Type)
				}
				out.Content = append(out.Content, serverBlock(b.Type, rawContent, i))
			}
		}
	}
	return out, nil
}

func mustContent(body []byte) []byte {
	var wrap struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &wrap) != nil || len(wrap.Content) == 0 {
		return []byte("[]")
	}
	return wrap.Content
}

// EncodeResponse renders the IR as an Anthropic Messages response. Used when
// Anthropic is the ingress format.
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	id := resp.ID
	if id == "" {
		id = ir.NewID("msg_")
	}
	content := make([]anthBlock, 0, len(resp.Content))
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockReasoning:
			// Opaque-only Responses items are not empty Anthropic thinking blocks.
			if b.ResponsesReasoning != nil && b.Text == "" && b.AnthropicSignature == "" {
				continue
			}
			content = append(content, anthBlock{Type: "thinking", Thinking: thinkingPtr(b.Text), Signature: b.AnthropicSignature})
		case ir.BlockRedactedReasoning:
			content = append(content, anthBlock{Type: "redacted_thinking", Data: b.RedactedData})
		case ir.BlockText:
			citations, err := encodeCitations(b.Citations)
			if err != nil {
				return nil, err
			}
			content = append(content, anthBlock{Type: "text", Text: b.Text, Citations: citations})
		case ir.BlockToolUse:
			input := b.ToolInput
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			content = append(content, anthBlock{Type: "tool_use", ID: b.ToolID, Name: b.ToolName, Input: input})
		case ir.BlockServer:
			raw, err := serverBlockRaw(b.AnthropicServer)
			if err != nil {
				return nil, err
			}
			content = append(content, raw)
		}
	}
	out := messagesResponse{
		ID:           id,
		Type:         "message",
		Role:         "assistant",
		Model:        resp.Model,
		Content:      content,
		StopReason:   stopReasonWire(resp.StopReason),
		StopSequence: resp.StopSequence,
		Usage:        usageToAnth(resp.Usage),
	}
	if resp.Anthropic != nil {
		state, err := validateResponseState(resp.Anthropic.Container, resp.Anthropic.ContextManagement, resp.Anthropic.StopDetails, resp.Anthropic.InputTransformations)
		if err != nil {
			return nil, ir.InvalidRequest("response state is malformed")
		}
		if state != nil {
			out.Container = state.Container
			out.ContextManagement = state.ContextManagement
			out.StopDetails = state.StopDetails
			out.InputTransformations = state.InputTransformations
		}
	}
	return json.Marshal(out)
}

func serverBlock(wireType string, raw []json.RawMessage, i int) ir.ContentBlock {
	var body json.RawMessage
	if i >= 0 && i < len(raw) {
		body = raw[i]
	}
	return ir.ContentBlock{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{WireType: wireType, Raw: body}}
}

func usageFromAnth(u anthUsage) ir.Usage {
	return ir.Usage{
		InputTokens:      u.TotalInput(),
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}.Clamped()
}

func usageToAnth(u ir.Usage) anthUsage {
	u = u.Clamped()
	out := anthUsage{OutputTokens: u.OutputTokens}
	if u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 {
		out.InputTokens = u.InputTokens
		return out
	}
	out.InputTokens = u.OrdinaryInputTokens()
	out.CacheReadInputTokens = u.CacheReadTokens
	out.CacheCreationInputTokens = u.CacheWriteTokens
	return out
}

func stopReason(s string) ir.StopReason {
	switch s {
	case "max_tokens":
		return ir.StopMaxTokens
	case "tool_use":
		return ir.StopToolUse
	case "stop_sequence":
		return ir.StopStopSequence
	case "refusal":
		return ir.StopRefusal
	case "pause_turn":
		return ir.StopPauseTurn
	default: // end_turn and unknown reasons stay end_turn for non-Anthropic projection
		return ir.StopEndTurn
	}
}

func stopReasonWire(sr ir.StopReason) string {
	switch sr {
	case ir.StopMaxTokens:
		return "max_tokens"
	case ir.StopToolUse:
		return "tool_use"
	case ir.StopStopSequence:
		return "stop_sequence"
	case ir.StopRefusal, ir.StopContentFilter:
		// Anthropic has no content-filter stop. Project that truncation to the
		// refusal vocabulary without rewriting already emitted text.
		return "refusal"
	case ir.StopPauseTurn:
		return "pause_turn"
	default:
		return "end_turn"
	}
}
