package proxy

import (
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
