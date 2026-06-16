package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"airouter/internal/proxy/ir"
)

// DecodeRequest parses an OpenAI Chat Completions request body into the IR.
// Used when OpenAI is the ingress format.
func DecodeRequest(body []byte) (*ir.Request, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("openai: decode request: %w", err)
	}

	out := &ir.Request{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	if req.MaxCompletionTokens != nil {
		out.MaxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	}
	out.StopSequences = parseStop(req.Stop)
	out.Tools = decodeTools(req.Tools)
	out.ToolChoice = decodeToolChoice(req.ToolChoice)

	var systemParts []string
	// toolCarrier accumulates consecutive role:"tool" messages into a single
	// user message of tool_result blocks (Anthropic's required shape).
	var toolCarrier *ir.Message
	flushCarrier := func() { toolCarrier = nil }

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			flushCarrier()
			if s := contentToText(m.Content); s != "" {
				systemParts = append(systemParts, s)
			}
		case "user":
			flushCarrier()
			out.Messages = append(out.Messages, ir.Message{
				Role:    ir.RoleUser,
				Content: decodeUserContent(m.Content),
			})
		case "assistant":
			flushCarrier()
			out.Messages = append(out.Messages, ir.Message{
				Role:    ir.RoleAssistant,
				Content: decodeAssistantContent(m),
			})
		case "tool":
			block := ir.ContentBlock{
				Type:       ir.BlockToolResult,
				ToolUseID:  m.ToolCallID,
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: contentToText(m.Content)}},
			}
			if toolCarrier == nil {
				out.Messages = append(out.Messages, ir.Message{Role: ir.RoleUser})
				toolCarrier = &out.Messages[len(out.Messages)-1]
			}
			toolCarrier.Content = append(toolCarrier.Content, block)
		}
	}
	out.System = strings.Join(systemParts, "\n\n")
	return out, nil
}

func decodeUserContent(raw json.RawMessage) []ir.ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	// Content is either a bare string or an array of typed parts.
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []ir.ContentBlock{{Type: ir.BlockText, Text: s}}
		}
		return nil
	}
	var parts []chatPart
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var blocks []ir.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: p.Text})
		case "image_url":
			if p.ImageURL != nil {
				blocks = append(blocks, ir.ContentBlock{Type: ir.BlockImage, Image: imageFromURL(p.ImageURL.URL)})
			}
		}
	}
	return blocks
}

func decodeAssistantContent(m chatMessage) []ir.ContentBlock {
	var blocks []ir.ContentBlock
	if t := contentToText(m.Content); t != "" {
		blocks = append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: t})
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, ir.ContentBlock{
			Type:      ir.BlockToolUse,
			ToolID:    tc.ID,
			ToolName:  tc.Function.Name,
			ToolInput: rawOrNull(tc.Function.Arguments),
		})
	}
	return blocks
}

func decodeTools(tools []chatTool) []ir.Tool {
	var out []ir.Tool
	for _, t := range tools {
		out = append(out, ir.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
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
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: obj.Function.Name}
}

// EncodeRequest renders the IR as an OpenAI Chat Completions request body. Used
// when OpenAI is the backend format.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	out := chatRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	// OpenAI omits the usage object from streaming responses unless asked, so
	// request a trailing usage-only chunk to recover token counts for logging.
	if req.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		out.MaxTokens = &mt
	}
	if len(req.StopSequences) > 0 {
		out.Stop, _ = json.Marshal(req.StopSequences)
	}
	if req.System != "" {
		sys := req.System
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: mustText(sys)})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, encodeMessage(m)...)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, chatTool{
			Type: "function",
			Function: chatFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if req.ToolChoice != nil {
		out.ToolChoice = encodeToolChoice(req.ToolChoice)
	}
	return json.Marshal(out)
}

// encodeMessage expands one IR message into one or more OpenAI messages. A user
// message carrying tool_result blocks becomes separate role:"tool" messages
// (emitted first) plus, if present, a user message for any text/image blocks.
func encodeMessage(m ir.Message) []chatMessage {
	if m.Role == ir.RoleAssistant {
		msg := chatMessage{Role: "assistant"}
		var text strings.Builder
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				text.WriteString(b.Text)
			case ir.BlockToolUse:
				msg.ToolCalls = append(msg.ToolCalls, chatToolCall{
					ID:   b.ToolID,
					Type: "function",
					Function: chatFunctionCall{
						Name:      b.ToolName,
						Arguments: string(rawOrNull(string(b.ToolInput))),
					},
				})
			}
		}
		if t := text.String(); t != "" {
			msg.Content = mustText(t)
		}
		return []chatMessage{msg}
	}

	// user role
	var out []chatMessage
	var parts []chatPart
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockToolResult:
			out = append(out, chatMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    mustText(toolResultText(b)),
			})
		case ir.BlockText:
			parts = append(parts, chatPart{Type: "text", Text: b.Text})
		case ir.BlockImage:
			parts = append(parts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: imageToURL(b.Image)}})
		}
	}
	if len(parts) > 0 {
		raw, _ := json.Marshal(parts)
		out = append(out, chatMessage{Role: "user", Content: raw})
	}
	return out
}

func encodeToolChoice(tc *ir.ToolChoice) json.RawMessage {
	switch tc.Type {
	case ir.ToolChoiceAuto:
		return mustText("auto")
	case ir.ToolChoiceNone:
		return mustText("none")
	case ir.ToolChoiceAny:
		return mustText("required")
	case ir.ToolChoiceTool:
		obj := map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
		raw, _ := json.Marshal(obj)
		return raw
	}
	return nil
}
