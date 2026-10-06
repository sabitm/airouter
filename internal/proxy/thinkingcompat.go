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

const (
	skipLogThinking = "thinking_incompatible"

	thinkingHistoryUnsupported = "signed or redacted thinking history is not supported by this upstream format"
	thinkingHistoryNoProvider  = "signed or redacted thinking history is not supported by any provider in combo"
)

func preservesAnthropicThinking(codecID string) bool {
	switch codecID {
	case codecAnthropic, codecClaudeCode, codecOpencodeMessages:
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

// noteOpaqueOmission records one payload-free diagnostic per HTTP request
// when a non-Anthropic ingress drops signature bytes or redacted block content.
// Empty lifecycle frames are not omissions. res is shared across failover
// attempts. logger may be nil. The caller must not pass the omitted bytes.
func noteOpaqueOmission(res *reqResult, logger *slog.Logger, ingressID string) {
	if res == nil || res.opaqueOmitted || preservesAnthropicThinking(ingressID) {
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
