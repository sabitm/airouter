package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
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
		cc, err := cacheControlFromRaw(t.CacheControl)
		if err != nil {
			return nil, fmt.Errorf("anthropic: decode request: tool cache_control: %w", err)
		}
		out.Tools = append(out.Tools, ir.Tool{
			Name:         t.Name,
			Description:  t.Description,
			Parameters:   t.InputSchema,
			CacheControl: cc,
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
		content, err := decodeRequestBlocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("anthropic: decode request: message content cache_control: %w", err)
		}
		out.Messages = append(out.Messages, ir.Message{Role: role, Content: content})
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

// decodeBlocks parses a message content field (string or []anthBlock) to IR
// without cache hints. It is the nested-context decoder for tool_result
// content: a hint there is dropped, never hoisted, because the canonical
// tool-result blocks have no hint slot.
func decodeBlocks(raw json.RawMessage) []ir.ContentBlock {
	out, _ := decodeBlocksHints(raw, false)
	return out
}

// decodeRequestBlocks parses one top-level request message content value. It
// captures and validates explicit cache_control hints on the eligible block
// kinds. A malformed supported-position marker returns an error so the request
// is rejected instead of the message silently disappearing.
func decodeRequestBlocks(raw json.RawMessage) ([]ir.ContentBlock, error) {
	return decodeBlocksHints(raw, true)
}

// decodeBlocksHints is the shared string-or-array content decoder. withHints
// enables request-side hint capture; excluded block kinds and nested tool_result
// content never carry a hint. Malformed non-array content still decodes to no
// blocks without an error, preserving the legacy behavior.
func decodeBlocksHints(raw json.RawMessage, withHints bool) ([]ir.ContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []ir.ContentBlock{{Type: ir.BlockText, Text: s}}, nil
		}
		return nil, nil
	}
	var blocks []anthBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, nil
	}
	var out []ir.ContentBlock
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			// Empty thinking text is retained. A signature-only block is a valid
			// replay block, not disposable content.
			out = append(out, ir.ContentBlock{
				Type: ir.BlockReasoning, Text: thinkingText(b.Thinking), AnthropicSignature: b.Signature,
			})
		case "redacted_thinking":
			out = append(out, ir.ContentBlock{Type: ir.BlockRedactedReasoning, RedactedData: b.Data})
		case "text":
			cc, err := decodeCacheHint(b.CacheControl, withHints)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.ContentBlock{Type: ir.BlockText, Text: b.Text, CacheControl: cc})
		case "image":
			cc, err := decodeCacheHint(b.CacheControl, withHints)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.ContentBlock{Type: ir.BlockImage, Image: imageFromSource(b.Source), CacheControl: cc})
		case "document":
			cc, err := decodeCacheHint(b.CacheControl, withHints)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.ContentBlock{Type: ir.BlockFile, File: fileFromDocument(b), CacheControl: cc})
		case "tool_use":
			cc, err := decodeCacheHint(b.CacheControl, withHints)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.ContentBlock{
				Type:         ir.BlockToolUse,
				ToolID:       b.ID,
				ToolName:     b.Name,
				ToolInput:    b.Input,
				CacheControl: cc,
			})
		case "tool_result":
			cc, err := decodeCacheHint(b.CacheControl, withHints)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.ContentBlock{
				Type:         ir.BlockToolResult,
				ToolUseID:    b.ToolUseID,
				IsError:      b.IsError,
				ToolResult:   decodeBlocks(b.Content),
				CacheControl: cc,
			})
		}
	}
	return out, nil
}

// decodeCacheHint converts a wire marker only where the canonical request IR
// models one. Every excluded context passes enabled false and drops the value
// without validation, so a malformed excluded marker cannot fail the decode.
func decodeCacheHint(raw json.RawMessage, enabled bool) (*ir.CacheControl, error) {
	if !enabled {
		return nil, nil
	}
	return cacheControlFromRaw(raw)
}

// cacheControlFromRaw converts one wire cache_control value to the canonical
// hint. Absent and JSON null are nil. A present marker must be a JSON object
// with type exactly "ephemeral" and, when ttl is present and non-null, ttl
// exactly "5m" or "1h". Unknown object keys are ignored. Errors never echo
// caller values.
func cacheControlFromRaw(raw json.RawMessage) (*ir.CacheControl, error) {
	if len(raw) == 0 || bytes.Equal(raw, nullJSON) {
		return nil, nil
	}
	var wire struct {
		Type *string `json:"type"`
		TTL  *string `json:"ttl"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, errors.New("cache_control is malformed")
	}
	if wire.Type == nil || *wire.Type != "ephemeral" {
		return nil, errors.New(`cache_control.type must be "ephemeral"`)
	}
	cc := &ir.CacheControl{Type: "ephemeral"}
	if wire.TTL != nil {
		if *wire.TTL != "5m" && *wire.TTL != "1h" {
			return nil, errors.New(`cache_control.ttl must be "5m" or "1h"`)
		}
		cc.TTL = *wire.TTL
	}
	return cc, nil
}

var nullJSON = []byte("null")

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
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.Parameters,
			CacheControl: cacheControlRaw(t.CacheControl),
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

// encodeBlocks renders top-level request content blocks, including explicit
// caller cache hints on the eligible block kinds.
func encodeBlocks(blocks []ir.ContentBlock) []anthBlock {
	return encodeBlocksHints(blocks, true)
}

// encodeToolResultBlocks renders nested tool_result content. Hints are always
// suppressed: a nested IR hint is dropped, never hoisted onto the outer block.
func encodeToolResultBlocks(blocks []ir.ContentBlock) []anthBlock {
	return encodeBlocksHints(blocks, false)
}

// encodeBlocksHints renders blocks. A hint is emitted only when hints are
// allowed and the block kind has a request-side marker slot; reasoning and
// redacted reasoning never emit one even when the IR was hand-built with it.
func encodeBlocksHints(blocks []ir.ContentBlock, hints bool) []anthBlock {
	out := make([]anthBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockReasoning:
			out = append(out, anthBlock{Type: "thinking", Thinking: thinkingPtr(b.Text), Signature: b.AnthropicSignature})
		case ir.BlockRedactedReasoning:
			out = append(out, anthBlock{Type: "redacted_thinking", Data: b.RedactedData})
		case ir.BlockText:
			out = append(out, anthBlock{Type: "text", Text: b.Text, CacheControl: cacheHintRaw(b.CacheControl, hints)})
		case ir.BlockImage:
			out = append(out, anthBlock{Type: "image", Source: sourceFromImage(b.Image), CacheControl: cacheHintRaw(b.CacheControl, hints)})
		case ir.BlockFile:
			// Only PDF documents are representable on Anthropic. Non-PDF files are
			// skipped here; preflight must reject incompatible targets before encode.
			if isPDFDocument(b.File) {
				blk := anthBlock{Type: "document", Source: sourceFromFile(b.File), Title: b.File.Filename, CacheControl: cacheHintRaw(b.CacheControl, hints)}
				out = append(out, blk)
			}
		case ir.BlockToolUse:
			input := b.ToolInput
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out = append(out, anthBlock{Type: "tool_use", ID: b.ToolID, Name: b.ToolName, Input: input, CacheControl: cacheHintRaw(b.CacheControl, hints)})
		case ir.BlockToolResult:
			content, _ := json.Marshal(encodeToolResultBlocks(b.ToolResult))
			out = append(out, anthBlock{Type: "tool_result", ToolUseID: b.ToolUseID, IsError: b.IsError, Content: content, CacheControl: cacheHintRaw(b.CacheControl, hints)})
		}
	}
	return out
}

// cacheHintRaw converts a canonical hint to wire bytes only at an allowed
// position. An empty TTL is omitted so a caller marker with no ttl round-trips
// without an implicit one.
func cacheHintRaw(cc *ir.CacheControl, enabled bool) json.RawMessage {
	if !enabled || cc == nil {
		return nil
	}
	return cacheControlRaw(cc)
}

func cacheControlRaw(cc *ir.CacheControl) json.RawMessage {
	if cc == nil {
		return nil
	}
	raw, err := json.Marshal(anthCacheControl{Type: cc.Type, TTL: cc.TTL})
	if err != nil {
		return nil
	}
	return raw
}
