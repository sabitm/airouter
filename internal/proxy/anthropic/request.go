package anthropic

import (
	"encoding/json"
	"fmt"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/thinking"
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
	var thType string
	var budget int
	if req.Thinking != nil {
		thType = req.Thinking.Type
		budget = req.Thinking.BudgetTokens
	}
	var effort string
	if req.OutputConfig != nil {
		effort = req.OutputConfig.Effort
	}
	out.Thinking = thinking.ToIR(thinking.FromAnthropic(thType, budget, effort))
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
		case "document":
			out = append(out, ir.ContentBlock{Type: ir.BlockFile, File: fileFromDocument(b)})
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
	return encodeRequest(req, domain.ProtocolAnthropic)
}

// EncodeRequestClaudeCode is EncodeRequest under ProtocolClaudeCode. Adaptive
// vs budget follows the model name (Haiku=budget; Opus/Sonnet 4.6+=adaptive).
func EncodeRequestClaudeCode(req *ir.Request) ([]byte, error) {
	return encodeRequest(req, domain.ProtocolClaudeCode)
}

func encodeRequest(req *ir.Request, protocol domain.Protocol) ([]byte, error) {
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
	applyThinking(&out, req, protocol)
	return json.Marshal(out)
}

// applyThinking writes Anthropic thinking fields from IR. Anthropic rejects
// thinking when the last message is not a user turn, so intent is dropped then.
// Budget thinking can exceed a small max_tokens; bump max_tokens when needed.
func applyThinking(out *messagesRequest, req *ir.Request, protocol domain.Protocol) {
	// Claude dialect is the transport default for Anthropic / Claude Code.
	// Provider-level overrides are applied by the proxy finalizer after encode.
	caps := thinking.CapsFor(req.Model, protocol, domain.ReasoningClaude)
	cfg := thinking.Effective(thinking.FromIR(req.Thinking), caps)
	if cfg == nil {
		return
	}
	if n := len(req.Messages); n == 0 || req.Messages[n-1].Role != ir.RoleUser {
		return
	}
	if cfg.Mode == thinking.ModeNone {
		out.Thinking = &anthThinking{Type: "disabled"}
		return
	}
	if caps.Format == thinking.FormatClaudeAdaptive {
		out.Thinking = &anthThinking{Type: "adaptive"}
		level := cfg.Level
		switch cfg.Mode {
		case thinking.ModeBudget:
			level = thinking.BudgetToLevel(cfg.Budget)
		case thinking.ModeAuto:
			level = "auto"
		}
		// Adaptive set: minimal->low, xhigh->high; max is preserved (not collapsed).
		level = thinking.MapClaudeAdaptiveLevel(level)
		if level == "" {
			level = "medium"
		}
		out.OutputConfig = &anthOutputConfig{Effort: level}
		return
	}
	if cfg.Mode == thinking.ModeAuto {
		out.Thinking = &anthThinking{Type: "enabled"}
		return
	}
	budget := thinking.BudgetFor(cfg, caps.BudgetMin, caps.BudgetMax)
	if budget <= 0 {
		budget = 8192
	}
	if caps.MaxOutput > 0 && budget+1024 > caps.MaxOutput {
		budget = max(1024, caps.MaxOutput-1024)
	}
	out.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
	if out.MaxTokens < budget+1024 {
		out.MaxTokens = budget + 1024
	}
	if caps.MaxOutput > 0 && out.MaxTokens > caps.MaxOutput {
		out.MaxTokens = caps.MaxOutput
	}
}

func encodeBlocks(blocks []ir.ContentBlock) []anthBlock {
	out := make([]anthBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			out = append(out, anthBlock{Type: "text", Text: b.Text})
		case ir.BlockImage:
			out = append(out, anthBlock{Type: "image", Source: sourceFromImage(b.Image)})
		case ir.BlockFile:
			// Only PDF documents are representable on Anthropic. Non-PDF files are
			// skipped here; preflight must reject incompatible targets before encode.
			if isPDFDocument(b.File) {
				blk := anthBlock{Type: "document", Source: sourceFromFile(b.File), Title: b.File.Filename}
				out = append(out, blk)
			}
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
