package responses

import (
	"encoding/json"
	"fmt"

	"airouter/internal/proxy/ir"
)

// DecodeRequest parses an OpenAI Responses request into the IR.
func DecodeRequest(body []byte) (*ir.Request, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("responses: decode request: %w", err)
	}
	out := &ir.Request{
		Model:       req.Model,
		System:      req.Instructions,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	if req.MaxOutputTokens != nil {
		out.MaxTokens = *req.MaxOutputTokens
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // only function tools are translated
		}
		out.Tools = append(out.Tools, ir.Tool{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	out.ToolChoice = decodeToolChoice(req.ToolChoice)
	out.Messages = decodeInput(req.Input, &out.System)
	return out, nil
}

// decodeInput converts the Responses `input` (string or item array) to IR
// messages. system/developer message items are folded into systemOut.
func decodeInput(raw json.RawMessage, systemOut *string) []ir.Message {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: s}}}}
		}
		return nil
	}
	var items []inputItem
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}

	var msgs []ir.Message
	// appendBlock attaches a block to a trailing message of the wanted role,
	// creating one if the last message is a different role. This groups adjacent
	// function_call blocks into one assistant turn and function_call_output
	// blocks into one user turn, matching Anthropic's required shape.
	appendBlock := func(role ir.Role, b ir.ContentBlock) {
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, b)
			return
		}
		msgs = append(msgs, ir.Message{Role: role, Content: []ir.ContentBlock{b}})
	}

	for _, it := range items {
		switch it.Type {
		case "", "message":
			if it.Role == "system" || it.Role == "developer" {
				if s := contentToText(it.Content); s != "" {
					if *systemOut != "" {
						*systemOut += "\n\n"
					}
					*systemOut += s
				}
				continue
			}
			role := ir.RoleUser
			if it.Role == "assistant" {
				role = ir.RoleAssistant
			}
			msgs = append(msgs, ir.Message{Role: role, Content: decodeParts(it.Content)})
		case "function_call":
			appendBlock(ir.RoleAssistant, ir.ContentBlock{
				Type: ir.BlockToolUse, ToolID: it.CallID, ToolName: it.Name, ToolInput: rawArgs(it.Arguments),
			})
		case "function_call_output":
			appendBlock(ir.RoleUser, ir.ContentBlock{
				Type:       ir.BlockToolResult,
				ToolUseID:  it.CallID,
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: outputToText(it.Output)}},
			})
		}
	}
	return msgs
}

func decodeParts(raw json.RawMessage) []ir.ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []ir.ContentBlock{{Type: ir.BlockText, Text: s}}
		}
		return nil
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var blocks []ir.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			blocks = append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: p.Text})
		case "input_image":
			if url := imageURLString(p.ImageURL); url != "" {
				blocks = append(blocks, ir.ContentBlock{Type: ir.BlockImage, Image: imageFromURL(url)})
			}
		}
	}
	return blocks
}

func decodeToolChoice(raw json.RawMessage) *ir.ToolChoice {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		switch s {
		case "auto":
			return &ir.ToolChoice{Type: ir.ToolChoiceAuto}
		case "none":
			return &ir.ToolChoice{Type: ir.ToolChoiceNone}
		case "required":
			return &ir.ToolChoice{Type: ir.ToolChoiceAny}
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: obj.Name}
}
