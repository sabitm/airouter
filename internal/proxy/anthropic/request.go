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
	system, systemBlocks, err := decodeSystem(req.System)
	if err != nil {
		return nil, fmt.Errorf("anthropic: decode request: %w", err)
	}
	out := &ir.Request{
		Model:         req.Model,
		System:        system,
		SystemBlocks:  systemBlocks,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
	}
	if err := attachAnthropicRequest(out, req); err != nil {
		return nil, fmt.Errorf("anthropic: decode request: %w", err)
	}
	for _, t := range req.Tools {
		cc, err := cacheControlFromRaw(t.CacheControl)
		if err != nil {
			return nil, fmt.Errorf("anthropic: decode request: tool cache_control: %w", err)
		}
		tool := ir.Tool{
			Name:         t.Name,
			Description:  t.Description,
			Parameters:   t.InputSchema,
			CacheControl: cc,
			Strict:       t.Strict,
		}
		if t.Type != "" || t.EagerInputStreaming != nil || t.DeferLoading != nil || t.MaxUses != nil || len(t.AllowedDomains) > 0 || len(t.BlockedDomains) > 0 {
			tool.AnthropicTool = &ir.AnthropicTool{
				Type:           t.Type,
				EagerInput:     t.EagerInputStreaming,
				DeferLoading:   t.DeferLoading,
				MaxUses:        t.MaxUses,
				AllowedDomains: append([]string(nil), t.AllowedDomains...),
				BlockedDomains: append([]string(nil), t.BlockedDomains...),
			}
		}
		if cc != nil {
			noteCachePolicy(out)
		}
		out.Tools = append(out.Tools, tool)
	}
	if req.ToolChoice != nil {
		out.ToolChoice = &ir.ToolChoice{
			Type:            ir.ToolChoiceType(req.ToolChoice.Type),
			Name:            req.ToolChoice.Name,
			DisableParallel: req.ToolChoice.DisableParallelToolUse,
		}
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
		if blocksHaveCache(content) {
			noteCachePolicy(out)
		}
		out.Messages = append(out.Messages, ir.Message{Role: role, Content: content})
	}
	for _, b := range systemBlocks {
		if b.CacheControl != nil {
			noteCachePolicy(out)
			break
		}
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
	if out.Thinking != nil && req.Thinking != nil {
		out.Thinking.Display = req.Thinking.Display
	} else if req.Thinking != nil && req.Thinking.Display != "" {
		if out.Thinking == nil {
			out.Thinking = &ir.Thinking{Display: req.Thinking.Display}
		}
	}
	return out, nil
}

func noteCachePolicy(out *ir.Request) {
	if out.Anthropic == nil {
		out.Anthropic = &ir.AnthropicRequest{}
	}
	out.Anthropic.CachePolicySet = true
}

func blocksHaveCache(blocks []ir.ContentBlock) bool {
	for _, b := range blocks {
		if b.CacheControl != nil {
			return true
		}
		// search_result is preserved as a server object, but its cache_control
		// is still caller policy; marking it prevents a synthesized anchor.
		if b.Type == ir.BlockServer && b.AnthropicServer != nil && serverRawHasCacheControl(b.AnthropicServer.Raw) {
			return true
		}
	}
	return false
}

func serverRawHasCacheControl(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	_, ok := obj["cache_control"]
	return ok
}

func attachAnthropicRequest(out *ir.Request, req messagesRequest) error {
	top, err := cacheControlFromRaw(req.CacheControl)
	if err != nil {
		return fmt.Errorf("top-level cache_control: %w", err)
	}
	metadata, err := validateOptionalObject(req.Metadata, "metadata")
	if err != nil {
		return err
	}
	contextManagement, err := validateOptionalObject(req.ContextManagement, "context_management")
	if err != nil {
		return err
	}
	if err := requireContextManagement(req.ContextManagement); err != nil {
		return err
	}
	if contextManagement != nil && !contextManagementHasEdits(contextManagement) {
		contextManagement = nil
	}
	var format json.RawMessage
	var budget *ir.AnthropicTaskBudget
	if req.OutputConfig != nil && len(req.OutputConfig.Format) > 0 {
		if err := requireOutputFormat(req.OutputConfig.Format); err != nil {
			return err
		}
		format = req.OutputConfig.Format
	}
	if req.OutputConfig != nil {
		budget, err = validateTaskBudget(req.OutputConfig.TaskBudget)
		if err != nil {
			return err
		}
	}
	fallbacks, err := validateFallbacks(req.Fallbacks)
	if err != nil {
		return err
	}
	container, err := validateContainer(req.Container)
	if err != nil {
		return err
	}
	if top == nil && metadata == nil && contextManagement == nil && len(format) == 0 && budget == nil && len(fallbacks) == 0 && len(container) == 0 {
		return nil
	}
	out.Anthropic = &ir.AnthropicRequest{
		Metadata:          metadata,
		ContextManagement: contextManagement,
		OutputFormat:      format,
		TaskBudget:        budget,
		Fallbacks:         fallbacks,
		Container:         container,
		TopLevelCache:     top,
		CachePolicySet:    top != nil,
	}
	return nil
}

func requireObject(raw json.RawMessage, name string) error {
	if len(raw) == 0 || bytes.Equal(raw, nullJSON) {
		return nil
	}
	if raw[0] != '{' || !json.Valid(raw) {
		return errors.New(name + " must be an object")
	}
	return nil
}

func requireContextManagement(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(raw, nullJSON) {
		return nil
	}
	if err := requireObject(raw, "context_management"); err != nil {
		return err
	}
	var obj struct {
		Edits json.RawMessage `json:"edits"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return errors.New("context_management must be an object")
	}
	if len(obj.Edits) > 0 && obj.Edits[0] != '[' {
		return errors.New("context_management.edits must be an array")
	}
	return nil
}

func requireOutputFormat(raw json.RawMessage) error {
	if err := requireObject(raw, "output_config.format"); err != nil {
		return err
	}
	var obj struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if json.Unmarshal(raw, &obj) != nil || obj.Type == "" {
		return errors.New("output_config.format must be an object")
	}
	if obj.Type == "json_schema" && (len(obj.Schema) == 0 || obj.Schema[0] != '{') {
		return errors.New("output_config.format.schema must be an object")
	}
	return nil
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
	var rawBlocks []json.RawMessage
	if json.Unmarshal(raw, &rawBlocks) != nil {
		return nil, nil
	}
	var out []ir.ContentBlock
	for i, rawBlock := range rawBlocks {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rawBlock, &head) != nil {
			return nil, fmt.Errorf("content block %d is malformed", i)
		}
		// Server blocks keep the original object. A typed decode would fail on
		// fields whose shape differs between block kinds, such as the
		// search_result source string versus an image source object.
		if knownServerBlockType(head.Type) && head.Type != "tool_result" {
			if !json.Valid(rawBlock) || len(rawBlock) == 0 || rawBlock[0] != '{' {
				return nil, fmt.Errorf("server block %d is malformed", i)
			}
			out = append(out, ir.ContentBlock{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{
				WireType: head.Type, Raw: append(json.RawMessage(nil), rawBlock...),
			}})
			continue
		}
		var b anthBlock
		if err := json.Unmarshal(rawBlock, &b); err != nil {
			return nil, fmt.Errorf("content block %d is malformed", i)
		}
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
			citations, err := decodeCitations(b.Citations)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.ContentBlock{Type: ir.BlockText, Text: b.Text, CacheControl: cc, Citations: citations})
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
			nested, err := decodeBlocksHints(b.Content, false)
			if err != nil {
				return nil, fmt.Errorf("tool_result block %d: %w", i, err)
			}
			out = append(out, ir.ContentBlock{
				Type:         ir.BlockToolResult,
				ToolUseID:    b.ToolUseID,
				IsError:      b.IsError,
				ToolResult:   nested,
				CacheControl: cc,
			})
		default:
			if b.Type != "" {
				// Unknown content must not disappear. Fail closed rather than
				// forwarding an arbitrary raw object to an Anthropic target.
				return nil, fmt.Errorf("unsupported content block type %q", b.Type)
			}
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
		Type  *string `json:"type"`
		TTL   *string `json:"ttl"`
		Scope *string `json:"scope"`
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
	if wire.Scope != nil {
		if *wire.Scope != "global" && *wire.Scope != "" {
			return nil, errors.New(`cache_control.scope must be "global"`)
		}
		cc.Scope = *wire.Scope
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
	if blocks := systemBlocksForEncode(req); blocks != nil {
		raw, err := json.Marshal(blocks)
		if err != nil {
			return nil, fmt.Errorf("anthropic: encode system: %w", err)
		}
		out.System = raw
	} else if req.System != "" {
		raw, err := json.Marshal(req.System)
		if err != nil {
			return nil, fmt.Errorf("anthropic: encode system: %w", err)
		}
		out.System = raw
	}
	for i, t := range req.Tools {
		// Schema syntax is validated here; the shape (object with type/properties)
		// is not, so a valid non-object schema still passes through unchanged.
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			return nil, ir.InvalidRequest(fmt.Sprintf("invalid tool schema at tool %d", i))
		}
		wire := anthTool{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.Parameters,
			CacheControl: cacheControlRaw(t.CacheControl),
			Strict:       t.Strict,
		}
		if t.AnthropicTool != nil {
			wire.Type = t.AnthropicTool.Type
			wire.EagerInputStreaming = t.AnthropicTool.EagerInput
			wire.DeferLoading = t.AnthropicTool.DeferLoading
			wire.MaxUses = t.AnthropicTool.MaxUses
			wire.AllowedDomains = t.AnthropicTool.AllowedDomains
			wire.BlockedDomains = t.AnthropicTool.BlockedDomains
		}
		out.Tools = append(out.Tools, wire)
	}
	if req.ToolChoice != nil {
		out.ToolChoice = &anthToolChoice{
			Type:                   string(req.ToolChoice.Type),
			Name:                   req.ToolChoice.Name,
			DisableParallelToolUse: req.ToolChoice.DisableParallel,
		}
	}
	if req.Anthropic != nil {
		metadata, err := validateOptionalObject(req.Anthropic.Metadata, "metadata")
		if err != nil {
			return nil, ir.InvalidRequest("metadata is malformed")
		}
		contextManagement, err := validateOptionalObject(req.Anthropic.ContextManagement, "context_management")
		if err != nil {
			return nil, ir.InvalidRequest("context_management is malformed")
		}
		if err := requireContextManagement(contextManagement); err != nil {
			return nil, ir.InvalidRequest(err.Error())
		}
		fallbacks, err := validateFallbacks(req.Anthropic.Fallbacks)
		if err != nil {
			return nil, ir.InvalidRequest("fallbacks are malformed")
		}
		container, err := validateContainer(req.Anthropic.Container)
		if err != nil {
			return nil, ir.InvalidRequest("container is malformed")
		}
		out.Metadata = metadata
		out.ContextManagement = contextManagement
		out.CacheControl = cacheControlRaw(req.Anthropic.TopLevelCache)
		out.Fallbacks = fallbacks
		out.Container = container
		if len(req.Anthropic.OutputFormat) > 0 {
			if out.OutputConfig == nil {
				out.OutputConfig = &anthOutputConfig{}
			}
			out.OutputConfig.Format = req.Anthropic.OutputFormat
		}
		if req.Anthropic.TaskBudget != nil {
			if req.Anthropic.TaskBudget.Total < 0 || (req.Anthropic.TaskBudget.Remaining != nil && *req.Anthropic.TaskBudget.Remaining < 0) {
				return nil, ir.InvalidRequest("output_config.task_budget values must not be negative")
			}
			if out.OutputConfig == nil {
				out.OutputConfig = &anthOutputConfig{}
			}
			out.OutputConfig.TaskBudget = encodeTaskBudget(req.Anthropic.TaskBudget)
		}
	}
	for i, m := range req.Messages {
		role := "user"
		if m.Role == ir.RoleAssistant {
			role = "assistant"
		}
		blocks, err := encodeBlocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		content, err := json.Marshal(blocks)
		if err != nil {
			return nil, fmt.Errorf("anthropic: encode message %d: %w", i, err)
		}
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
	display := ""
	if req.Thinking != nil {
		display = req.Thinking.Display
	}
	if cfg.Mode == thinking.ModeNone {
		if caps.OmitOnDisable {
			out.Thinking = nil
			if out.OutputConfig != nil {
				out.OutputConfig.Effort = ""
				if len(out.OutputConfig.Format) == 0 && out.OutputConfig.TaskBudget == nil {
					out.OutputConfig = nil
				}
			}
			return
		}
		// Disabled thinking carries no display or budget fields.
		out.Thinking = &anthThinking{Type: "disabled"}
		return
	}
	if caps.Format == thinking.FormatClaudeAdaptive {
		out.Thinking = &anthThinking{Type: "adaptive", Display: display}
		level := cfg.Level
		switch cfg.Mode {
		case thinking.ModeBudget:
			level = thinking.BudgetToLevel(cfg.Budget)
		case thinking.ModeAuto:
			level = "auto"
		}
		level = thinking.MapClaudeAdaptiveLevel(level, caps)
		if level == "" {
			level = "medium"
		}
		if out.OutputConfig == nil {
			out.OutputConfig = &anthOutputConfig{}
		}
		out.OutputConfig.Effort = level
		return
	}
	if cfg.Mode == thinking.ModeAuto {
		out.Thinking = &anthThinking{Type: "enabled", Display: display}
		return
	}
	budget := thinking.BudgetFor(cfg, caps.BudgetMin, caps.BudgetMax)
	if budget <= 0 {
		budget = 8192
	}
	if caps.MaxOutput > 0 && budget+1024 > caps.MaxOutput {
		budget = max(1024, caps.MaxOutput-1024)
	}
	out.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget, Display: display}
	if out.MaxTokens < budget+1024 {
		out.MaxTokens = budget + 1024
	}
	if caps.MaxOutput > 0 && out.MaxTokens > caps.MaxOutput {
		out.MaxTokens = caps.MaxOutput
	}
}

// encodeBlocks renders top-level request content blocks, including explicit
// caller cache hints on the eligible block kinds.
func encodeBlocks(blocks []ir.ContentBlock) ([]anthBlock, error) {
	return encodeBlocksHints(blocks, true)
}

// encodeToolResultBlocks renders nested tool_result content. Hints are always
// suppressed: a nested IR hint is dropped, never hoisted onto the outer block.
func encodeToolResultBlocks(blocks []ir.ContentBlock) ([]anthBlock, error) {
	return encodeBlocksHints(blocks, false)
}

// encodeBlocksHints renders blocks. A hint is emitted only when hints are
// allowed and the block kind has a request-side marker slot; reasoning and
// redacted reasoning never emit one even when the IR was hand-built with it.
// A non-empty tool input must be syntactically valid JSON; an invalid one is a
// payload-free client validation error so the whole assistant turn is not lost.
func encodeBlocksHints(blocks []ir.ContentBlock, hints bool) ([]anthBlock, error) {
	out := make([]anthBlock, 0, len(blocks))
	for i := range blocks {
		b := blocks[i]
		switch b.Type {
		case ir.BlockReasoning:
			// An opaque-only Responses item has no readable summary. Do not turn
			// it into an empty thinking block; a real Anthropic signature-only
			// block still emits.
			if b.ResponsesReasoning != nil && b.Text == "" && b.AnthropicSignature == "" {
				continue
			}
			out = append(out, anthBlock{Type: "thinking", Thinking: thinkingPtr(b.Text), Signature: b.AnthropicSignature})
		case ir.BlockRedactedReasoning:
			out = append(out, anthBlock{Type: "redacted_thinking", Data: b.RedactedData})
		case ir.BlockServer:
			raw, err := serverBlockRaw(b.AnthropicServer)
			if err != nil {
				return nil, err
			}
			out = append(out, raw)
		case ir.BlockText:
			citations, err := encodeCitations(b.Citations)
			if err != nil {
				return nil, err
			}
			out = append(out, anthBlock{Type: "text", Text: b.Text, Citations: citations, CacheControl: cacheHintRaw(b.CacheControl, hints)})
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
			} else if !json.Valid(input) {
				return nil, ir.InvalidRequest(fmt.Sprintf("invalid tool call arguments at block %d", i))
			}
			out = append(out, anthBlock{Type: "tool_use", ID: b.ToolID, Name: b.ToolName, Input: input, CacheControl: cacheHintRaw(b.CacheControl, hints)})
		case ir.BlockToolResult:
			nested, err := encodeToolResultBlocks(b.ToolResult)
			if err != nil {
				return nil, fmt.Errorf("tool_result block %d: %w", i, err)
			}
			content, err := json.Marshal(nested)
			if err != nil {
				return nil, fmt.Errorf("anthropic: encode tool_result block %d: %w", i, err)
			}
			out = append(out, anthBlock{Type: "tool_result", ToolUseID: b.ToolUseID, IsError: b.IsError, Content: content, CacheControl: cacheHintRaw(b.CacheControl, hints)})
		}
	}
	return out, nil
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
	raw, err := json.Marshal(anthCacheControl{Type: cc.Type, TTL: cc.TTL, Scope: cc.Scope})
	if err != nil {
		return nil
	}
	return raw
}

// serverBlockRaw keeps the original server object. Decoding it into anthBlock
// drops tool_name, server_name, and nested result content, so those fields
// must be copied from Raw. The object is checked only for a matching type.
func serverBlockRaw(s *ir.AnthropicServerBlock) (anthBlock, error) {
	if s == nil || len(s.Raw) == 0 || !json.Valid(s.Raw) || s.Raw[0] != '{' {
		return anthBlock{}, ir.InvalidRequest("server block is malformed")
	}
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(s.Raw, &probe) != nil || probe.Type == "" || (s.WireType != "" && probe.Type != s.WireType) {
		return anthBlock{}, ir.InvalidRequest("server block type is invalid")
	}
	if !knownServerBlockType(probe.Type) {
		return anthBlock{}, ir.InvalidRequest("server block type is unsupported")
	}
	return anthBlock{Raw: append(json.RawMessage(nil), s.Raw...)}, nil
}
