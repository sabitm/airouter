package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"

	"airouter/internal/proxy/ir"
)

// Anthropic-family codec IDs that can replay signed and redacted thinking.
// Capability is explicit. Provider protocol is not used to infer it.
const (
	codecAnthropic        = "anth-msg"
	codecClaudeCode       = "claude-code"
	codecOpencodeMessages = "opencode-messages"
)

// Responses-family codec IDs that can replay provider-owned reasoning IDs and
// encrypted_content. A Responses ingress may only reach one of these, and only
// when the single destination provider/model is unchanged.
const (
	codecResponsesCodec   = "oai-responses"
	codecCodex            = "oai-codex"
	codecOpencodeResponse = "opencode-responses"
)

const (
	skipLogThinking = "thinking_incompatible"

	thinkingHistoryUnsupported = "signed or redacted thinking history is not supported by this upstream format"
	thinkingHistoryNoProvider  = "signed or redacted thinking history is not supported by any provider in combo"

	responsesStateUnsupported = "provider-owned Responses reasoning is not supported by this upstream format"
	responsesStateNoProvider  = "provider-owned Responses reasoning is not supported by any provider in combo"
	// responsesStateAmbiguous is returned before any upstream contact when more
	// than one provider/model destination could receive opaque history.
	responsesStateAmbiguous = "provider-owned Responses reasoning is ambiguous across multiple providers or models in combo"
)

func preservesAnthropicThinking(codecID string) bool {
	switch codecID {
	case codecAnthropic, codecClaudeCode, codecOpencodeMessages:
		return true
	default:
		return false
	}
}

// preservesAnthropicExtensions reports whether the codec can carry typed
// server tools, server blocks, and Anthropic-only request fields. OpenCode
// Messages reuses the Anthropic encoder, so it carries the same fields.
func preservesAnthropicExtensions(codecID string) bool {
	switch codecID {
	case codecAnthropic, codecClaudeCode, codecOpencodeMessages:
		return true
	default:
		return false
	}
}

// preservesAnthropicOutputFormat reports whether the codec has a native or
// mapped slot for output_config.format. OpenAI and Responses map the supported
// json_schema shape; other backends have no representation.
func preservesAnthropicOutputFormat(codecID string) bool {
	switch codecID {
	case codecAnthropic, codecClaudeCode, codecOpencodeMessages,
		"oai-chat", "oai-responses", "oai-codex", "opencode-chat", "opencode-responses":
		return true
	default:
		return false
	}
}

// preservesStrictTools reports whether the codec accepts strict custom-tool
// intent. OpenAI and Responses emit native enforcement. Cursor accepts the
// tool but has no strict flag, so enforcement is not promised. Other targets
// must fail before send.
func preservesStrictTools(codecID string) bool {
	// Cursor accepts ordinary custom tools and their schemas. It has no native
	// strict flag, so strict is not enforced and is not promised there.
	return preservesAnthropicOutputFormat(codecID) || codecID == "cursor"
}

const (
	anthropicStateUnsupported        = "provider-owned Anthropic tool or request state is not supported by this upstream format"
	anthropicOutputFormatUnsupported = "output format is not supported by this upstream format"
	anthropicStrictToolsUnsupported  = "strict tool schemas are not supported by this upstream format"
	jsonObjectFormatUnsupported      = "json_object output format is not supported by this upstream format"
	strictFalseFormatUnsupported     = "strict=false json_schema output is not supported by this upstream format"
)

// outputFormatTargetReason applies per-format target support for portable
// structured-output intent. Same-source native passthrough is exempt so a shape
// this proxy does not model is still forwarded unchanged. OpenAI Chat and
// Responses can carry json_schema and json_object. Anthropic-family targets can
// carry json_schema only, and only when the caller did not ask for non-strict
// output. Codex is absent because its encoder does not emit text.format; it is
// filtered instead of silently dropping the caller requirement.
func outputFormatTargetReason(codecID string, f *ir.OutputFormat) string {
	if f == nil {
		return ""
	}
	if f.Source != "" && f.Source == codecID {
		return ""
	}
	if err := f.Validate(); err != nil {
		return err.Error()
	}
	switch f.Type {
	case ir.OutputFormatJSONSchema:
		switch codecID {
		case codecAnthropic, codecClaudeCode, codecOpencodeMessages:
			if f.Strict != nil && !*f.Strict {
				return strictFalseFormatUnsupported
			}
			return ""
		case "oai-chat", "oai-responses", "opencode-chat", "opencode-responses":
			return ""
		}
	case ir.OutputFormatJSONObject:
		switch codecID {
		case "oai-chat", "oai-responses", "opencode-chat", "opencode-responses":
			return ""
		case codecAnthropic, codecClaudeCode, codecOpencodeMessages:
			return jsonObjectFormatUnsupported
		}
	case ir.OutputFormatText:
		// Text is the unconstrained default; omitting the format represents it.
		return ""
	}
	return anthropicOutputFormatUnsupported
}

// requestAnthropicOwnedState reports caller state that must not be converted
// into a custom tool or dropped on a non-Anthropic backend. Portable metadata
// and strict custom-tool intent are not owned state; output format is handled
// by an explicit target capability. Empty metadata/context values are absent.
func requestAnthropicOwnedState(req *ir.Request) bool {
	if req == nil {
		return false
	}
	if req.Anthropic != nil {
		if anthropicContextManagementOwned(req.Anthropic.ContextManagement) {
			return true
		}
		if req.Anthropic.TaskBudget != nil || len(req.Anthropic.Fallbacks) > 0 || len(req.Anthropic.Container) > 0 {
			return true
		}
	}
	for _, t := range req.Tools {
		if t.AnthropicTool == nil {
			continue
		}
		at := t.AnthropicTool
		if at.Type != "" || at.MaxUses != nil || len(at.AllowedDomains) > 0 || len(at.BlockedDomains) > 0 {
			return true
		}
		if at.EagerInput != nil && *at.EagerInput {
			return true
		}
		if at.DeferLoading != nil && *at.DeferLoading {
			return true
		}
	}
	return anthropicServerInMessages(req.Messages)
}

// anthropicContextManagementOwned is true only when at least one edit is
// present. Null, {}, and {"edits":[]} are not provider-owned state.
func anthropicContextManagementOwned(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return false
	}
	var obj struct {
		Edits []json.RawMessage `json:"edits"`
	}
	if json.Unmarshal(trimmed, &obj) != nil {
		return true
	}
	return len(obj.Edits) > 0
}

func requestAnthropicOutputFormat(req *ir.Request) bool {
	return req != nil && req.Anthropic != nil && len(req.Anthropic.OutputFormat) > 0
}

func requestStrictTools(req *ir.Request) bool {
	if req == nil {
		return false
	}
	for _, t := range req.Tools {
		if t.Strict != nil && *t.Strict {
			return true
		}
	}
	return false
}

func anthropicServerInMessages(msgs []ir.Message) bool {
	var walk func([]ir.ContentBlock) bool
	walk = func(blocks []ir.ContentBlock) bool {
		for _, b := range blocks {
			if b.Type == ir.BlockServer || b.AnthropicServer != nil {
				return true
			}
			if b.Type == ir.BlockToolResult && walk(b.ToolResult) {
				return true
			}
		}
		return false
	}
	for _, m := range msgs {
		if walk(m.Content) {
			return true
		}
	}
	return false
}

// preservesResponsesReasoning reports whether the codec can replay the
// Responses reasoning ID and encrypted_content fields without interpreting
// them. The provider/model is still fixed by the route guard.
func preservesResponsesReasoning(codecID string) bool {
	switch codecID {
	case codecResponsesCodec, codecCodex, codecOpencodeResponse:
		return true
	default:
		return false
	}
}

// requestThinkingHistory reports whether decoded history contains Anthropic
// signed reasoning or redacted reasoning. Unsigned reasoning text does not
// require a preserving target.
func requestThinkingHistory(req *ir.Request) (signed, redacted bool) {
	if req == nil {
		return false, false
	}
	var walk func([]ir.ContentBlock)
	walk = func(blocks []ir.ContentBlock) {
		for _, b := range blocks {
			switch b.Type {
			case ir.BlockReasoning:
				if b.AnthropicSignature != "" {
					signed = true
				}
			case ir.BlockRedactedReasoning:
				redacted = true
			case ir.BlockToolResult:
				walk(b.ToolResult)
			}
		}
	}
	for i := range req.Messages {
		walk(req.Messages[i].Content)
	}
	return signed, redacted
}

func requiresAnthropicThinking(req *ir.Request) bool {
	signed, redacted := requestThinkingHistory(req)
	return signed || redacted
}

// requestResponsesOwnedState reports whether decoded history carries a
// provider-owned Responses reasoning item ID or explicit encrypted_content,
// including an empty string. Plain unsigned summary text is portable and does
// not require a preserving route.
func requestResponsesOwnedState(req *ir.Request) bool {
	if req == nil {
		return false
	}
	var walk func([]ir.ContentBlock) bool
	walk = func(blocks []ir.ContentBlock) bool {
		for i := range blocks {
			b := &blocks[i]
			switch b.Type {
			case ir.BlockReasoning:
				if b.ResponsesReasoning.ProviderState() {
					return true
				}
			case ir.BlockToolResult:
				if walk(b.ToolResult) {
					return true
				}
			}
		}
		return false
	}
	for i := range req.Messages {
		if walk(req.Messages[i].Content) {
			return true
		}
	}
	return false
}

// noteOpaqueOmission records one payload-free diagnostic per HTTP request when
// an ingress drops opaque bytes it cannot represent. The caller decides whether
// the event family is representable by that ingress. res is shared across
// failover attempts. logger may be nil. The caller must not pass the omitted
// bytes. At most one diagnostic is recorded per request across families.
func noteOpaqueOmission(res *reqResult, logger *slog.Logger, ingressID string) {
	if res == nil || res.opaqueOmitted {
		return
	}
	res.opaqueOmitted = true
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("thinking_opaque_omitted",
		"event", "thinking_opaque_omitted",
		"ingress", ingressID,
	)
}

// noteAnthropicOmission records an omission only when signed or redacted
// Anthropic bytes were actually dropped and the ingress cannot replay them.
func noteAnthropicOmission(res *reqResult, logger *slog.Logger, ingressID string) {
	if preservesAnthropicThinking(ingressID) {
		return
	}
	noteOpaqueOmission(res, logger, ingressID)
}

// noteResponsesOmission records an omission only when provider-owned Responses
// bytes were actually dropped and the ingress cannot replay them.
func noteResponsesOmission(res *reqResult, logger *slog.Logger, ingressID string) {
	if preservesResponsesReasoning(ingressID) {
		return
	}
	noteOpaqueOmission(res, logger, ingressID)
}

// opaqueThinkingOmitted reports whether ev carries signature or redacted bytes
// that a projecting ingress cannot represent. An empty signature fragment is
// not an omission. The function does not retain those bytes.
func opaqueThinkingOmitted(ev ir.StreamEvent) bool {
	switch ev.Kind {
	case ir.EventReasoningSignature:
		return ev.Signature != ""
	case ir.EventReasoningStart:
		return ev.Signature != ""
	case ir.EventRedactedReasoning:
		return ev.Data != ""
	default:
		return false
	}
}

// responsesOpaqueOmitted reports whether ev carries provider-owned Responses
// item state that a projecting ingress cannot represent. An item ID alone is
// continuity state, so it counts exactly like explicit encrypted content. The
// value itself is never retained by this function.
func responsesOpaqueOmitted(ev ir.StreamEvent) bool {
	return ev.Kind == ir.EventReasoningItemDone && ev.Reasoning.ProviderState()
}

func responseDropsOpaque(blocks []ir.ContentBlock) bool {
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockReasoning:
			if b.AnthropicSignature != "" {
				return true
			}
		case ir.BlockRedactedReasoning:
			return true
		}
	}
	return false
}

// responseDropsResponsesOpaque reports whether blocks carry a provider-owned
// Responses encrypted value or item ID with no readable projection the ingress
// can replay. Plain summary text does not count.
func responseDropsResponsesOpaque(blocks []ir.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == ir.BlockReasoning && b.ResponsesReasoning.ProviderState() {
			return true
		}
	}
	return false
}

func thinkingSkip(reason string) attemptResult {
	if reason == "" {
		reason = thinkingHistoryUnsupported
	}
	return attemptResult{
		retry:   true,
		status:  400,
		errMsg:  clampErrorMessage(reason),
		logErr:  skipLogThinking,
		errType: "invalid_request_error",
	}
}
