package anthropic

import (
	"encoding/json"
	"fmt"

	"airouter/internal/proxy/ir"
)

// DefaultMaxTokens is used when translating from a format that did not supply
// max_tokens, since the Anthropic Messages API requires it.
const DefaultMaxTokens = 4096

// DecodeRequest parses an Anthropic Messages request into the IR. Used when
// Anthropic is the ingress format.
func DecodeRequest(body []byte) (*ir.Request, error) {
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("anthropic: decode request: %w", err)
	}
	out := &ir.Request{
		Model:         req.Model,
		System:        systemToText(req.System),
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	if req.ToolChoice != nil {
		out.ToolChoice = &ir.ToolChoice{Type: ir.ToolChoiceType(req.ToolChoice.Type), Name: req.ToolChoice.Name}
	}
	for _, m := range req.Messages {
		role := ir.RoleUser
		if m.Role == "assistant" {
			role = ir.RoleAssistant
		}
		out.Messages = append(out.Messages, ir.Message{Role: role, Content: decodeBlocks(m.Content)})
	}
	return out, nil
}

// decodeBlocks parses a message content field (string or []anthBlock) to IR.
func decodeBlocks(raw json.RawMessage) []ir.ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []ir.ContentBlock{{Type: ir.BlockText, Text: s}}
		}
		return nil
	}
	var blocks []anthBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []ir.ContentBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, ir.ContentBlock{Type: ir.BlockText, Text: b.Text})
		case "image":
			out = append(out, ir.ContentBlock{Type: ir.BlockImage, Image: imageFromSource(b.Source)})
		case "tool_use":
			out = append(out, ir.ContentBlock{
				Type:      ir.BlockToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				ToolInput: b.Input,
			})
		case "tool_result":
			out = append(out, ir.ContentBlock{
				Type:       ir.BlockToolResult,
				ToolUseID:  b.ToolUseID,
				IsError:    b.IsError,
				ToolResult: decodeBlocks(b.Content),
			})
		}
	}
	return out
}

// EncodeRequest renders the IR as an Anthropic Messages request. Used when
// Anthropic is the backend format.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	out := messagesRequest{
		Model:         req.Model,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
		MaxTokens:     req.MaxTokens,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}
	if req.System != "" {
		out.System, _ = json.Marshal(req.System)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anthTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	if req.ToolChoice != nil {
		out.ToolChoice = &anthToolChoice{Type: string(req.ToolChoice.Type), Name: req.ToolChoice.Name}
	}
	for _, m := range req.Messages {
		role := "user"
		if m.Role == ir.RoleAssistant {
			role = "assistant"
		}
		content, _ := json.Marshal(encodeBlocks(m.Content))
		out.Messages = append(out.Messages, anthMessage{Role: role, Content: content})
	}
	return json.Marshal(out)
}

func encodeBlocks(blocks []ir.ContentBlock) []anthBlock {
	out := make([]anthBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			out = append(out, anthBlock{Type: "text", Text: b.Text})
		case ir.BlockImage:
			out = append(out, anthBlock{Type: "image", Source: sourceFromImage(b.Image)})
		case ir.BlockToolUse:
			input := b.ToolInput
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out = append(out, anthBlock{Type: "tool_use", ID: b.ToolID, Name: b.ToolName, Input: input})
		case ir.BlockToolResult:
			content, _ := json.Marshal(encodeBlocks(b.ToolResult))
			out = append(out, anthBlock{Type: "tool_result", ToolUseID: b.ToolUseID, IsError: b.IsError, Content: content})
		}
	}
	return out
}
