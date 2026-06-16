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
	out := &ir.Response{
		ID:         resp.ID,
		Model:      resp.Model,
		StopReason: stopReason(resp.StopReason),
		Usage:      ir.Usage{InputTokens: resp.Usage.TotalInput(), OutputTokens: resp.Usage.OutputTokens},
	}
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockText, Text: b.Text})
		case "tool_use":
			out.Content = append(out.Content, ir.ContentBlock{
				Type:      ir.BlockToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				ToolInput: b.Input,
			})
		}
	}
	return out, nil
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
		case ir.BlockText:
			content = append(content, anthBlock{Type: "text", Text: b.Text})
		case ir.BlockToolUse:
			input := b.ToolInput
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			content = append(content, anthBlock{Type: "tool_use", ID: b.ToolID, Name: b.ToolName, Input: input})
		}
	}
	out := messagesResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      resp.Model,
		Content:    content,
		StopReason: stopReasonWire(resp.StopReason),
		Usage:      anthUsage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
	}
	return json.Marshal(out)
}

func stopReason(s string) ir.StopReason {
	switch s {
	case "max_tokens":
		return ir.StopMaxTokens
	case "tool_use":
		return ir.StopToolUse
	case "stop_sequence":
		return ir.StopStopSequence
	default: // end_turn and others
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
	default:
		return "end_turn"
	}
}
