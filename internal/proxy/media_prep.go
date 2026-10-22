package proxy

import (
	"context"
	"fmt"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/media"
)

// attachmentPrep holds request-scoped attachment state shared across failover
// attempts. The original client body is always the source of truth; each attempt
// decodes fresh IR and materializes into that clone so mutations do not leak.
type attachmentPrep struct {
	atts      []media.Attachment
	inspected bool
	// preserveThinking is true when the decoded request carries signed or
	// redacted Anthropic thinking that a target must be able to replay.
	preserveThinking bool
	// preserveResponsesState is true when the decoded request carries a
	// provider-owned Responses reasoning ID or encrypted_content that must not
	// be retried at another provider or model.
	preserveResponsesState bool
	// preserveAnthropicState is true when the request carries typed server
	// tools, server blocks, or other Anthropic-only fields.
	preserveAnthropicState bool
	// preserveOutputFormat is true when the caller explicitly asked for a
	// structured output format. Targets must map it or reject before send.
	preserveOutputFormat bool
	// legacyOutputFormat is the Anthropic-owned output_config.format field.
	// portableOutputFormat is OpenAI Chat response_format or Responses
	// text.format. They stay separate until a conflict check proves equivalence.
	legacyOutputFormat   bool
	portableOutputFormat *ir.OutputFormat
	// outputFormatErr is a payload-free conflict or validation reason discovered
	// before target ordering.
	outputFormatErr string
	// preserveStrictTools is true when a caller asked for strict schema
	// enforcement, which a target must be able to express.
	preserveStrictTools bool
	// preserveCacheEvict is true when an eligible cache marker asked for
	// eviction on completion. Only an explicit true requires a preserving
	// target; ordinary hints keep the legacy drop behavior.
	preserveCacheEvict bool
	// rejectAnthropicSpeed is true when the caller sent a non-null Messages
	// speed value. Only the native Anthropic Messages codec may carry it.
	rejectAnthropicSpeed bool
	// inlineBytes is the decoded size of locally available payloads, recorded
	// once per request so materialize can charge remote bytes against the same
	// budget without re-summing IR clones.
	inlineBytes int
	// fetcher is optional; production leaves it nil so materialize constructs a
	// default. Tests inject a Fetcher (e.g. with a custom Client/Resolver).
	fetcher *media.Fetcher
}

func (a *attachmentPrep) inspectDecoded(req *ir.Request) error {
	if a == nil {
		return nil
	}
	atts, err := media.InspectRequest(req)
	if err != nil {
		return err
	}
	a.atts = atts
	a.inspected = true
	a.preserveThinking = requiresAnthropicThinking(req)
	a.preserveResponsesState = requestResponsesOwnedState(req)
	a.preserveAnthropicState = requestAnthropicOwnedState(req)
	a.legacyOutputFormat = requestAnthropicOutputFormat(req)
	a.portableOutputFormat = nil
	a.outputFormatErr = ""
	if req != nil {
		a.portableOutputFormat = req.OutputFormat
		if _, err := req.EffectivePortableOutputFormat(); err != nil {
			a.outputFormatErr = err.Error()
		}
	}
	a.preserveOutputFormat = a.legacyOutputFormat || a.portableOutputFormat != nil
	a.preserveStrictTools = requestStrictTools(req)
	a.preserveCacheEvict = requestCacheEviction(req)
	a.rejectAnthropicSpeed = requestAnthropicSpeed(req)
	n := 0
	for _, att := range atts {
		n += att.Bytes
	}
	a.inlineBytes = n
	return nil
}

func (a *attachmentPrep) hasAttachments() bool {
	return a != nil && len(a.atts) > 0
}

func (a *attachmentPrep) needsThinkingPreservation() bool {
	return a != nil && a.preserveThinking
}

func (a *attachmentPrep) needsResponsesPreservation() bool {
	return a != nil && a.preserveResponsesState
}

// needsAnthropicState reports request intent that must be filtered before
// target ordering: provider-owned state, explicit output format, strict
// custom tools, enabled cache eviction, or a Messages speed value.
func (a *attachmentPrep) needsAnthropicState() bool {
	return a != nil && (a.preserveAnthropicState || a.preserveOutputFormat || a.preserveStrictTools || a.preserveCacheEvict || a.rejectAnthropicSpeed)
}

// checkThinking returns a reason when backend cannot replay signed or redacted
// Anthropic thinking, or provider-owned Responses reasoning, present in the
// request. Anthropic and Responses capabilities are independent.
func (a *attachmentPrep) checkThinking(backend codec) string {
	if a == nil {
		return ""
	}
	if a.preserveThinking && !preservesAnthropicThinking(backend.id) {
		return thinkingHistoryUnsupported
	}
	if a.preserveResponsesState && !preservesResponsesReasoning(backend.id) {
		return responsesStateUnsupported
	}
	if a.preserveAnthropicState && !preservesAnthropicExtensions(backend.id) {
		return anthropicStateUnsupported
	}
	if reason := a.checkOutputFormat(backend.id); reason != "" {
		return reason
	}
	if a.preserveStrictTools && !preservesStrictTools(backend.id) {
		return anthropicStrictToolsUnsupported
	}
	if a.preserveCacheEvict && !preservesCacheEviction(backend.id) {
		return cacheEvictionUnsupported
	}
	if a.rejectAnthropicSpeed && !preservesAnthropicSpeed(backend.id) {
		return anthropicSpeedUnsupported
	}
	return ""
}

// checkOutputFormat applies per-format target support. A portable format with
// the same codec id is a native passthrough, so unknown or malformed native
// fields stay untouched. Legacy Anthropic output_config.format keeps its
// existing codec capability rule. A portable format on any other target must be
// validated and representable; otherwise the target is filtered before network.
func (a *attachmentPrep) checkOutputFormat(codecID string) string {
	if a == nil || !a.preserveOutputFormat {
		return ""
	}
	if a.outputFormatErr != "" {
		return a.outputFormatErr
	}
	if a.portableOutputFormat != nil {
		return outputFormatTargetReason(codecID, a.portableOutputFormat)
	}
	if !preservesAnthropicOutputFormat(codecID) {
		return anthropicOutputFormatUnsupported
	}
	return ""
}

// checkCompatible returns a non-empty reason when backend cannot represent the
// request's attachments. translated distinguishes passthrough vs IR translation
// for provider file-ID portability.
func (a *attachmentPrep) checkCompatible(backend codec, translated bool) string {
	if a == nil || len(a.atts) == 0 {
		return ""
	}
	return media.CapsForCodecID(backend.id).Incompatible(a.atts, translated)
}

// materialize fetches remote image URLs into inline Data on a per-attempt IR
// clone for backends that cannot accept URLs. Fetch failures are media errors
// (not provider-health failures).
func (a *attachmentPrep) materialize(ctx context.Context, req *ir.Request, backend codec) error {
	if a == nil || req == nil {
		return nil
	}
	caps := media.CapsForCodecID(backend.id)
	if !caps.MaterializeImageURL {
		return nil
	}
	if a.fetcher == nil {
		a.fetcher = &media.Fetcher{}
	}
	// Sum this clone's fetched bytes against the request inline total. The
	// shared Fetcher cache prevents repeat downloads across failover; this
	// per-walk sum avoids accumulating the same remote bytes onto the budget.
	total := a.inlineBytes
	var walk func(blocks []ir.ContentBlock) error
	walk = func(blocks []ir.ContentBlock) error {
		for i := range blocks {
			b := &blocks[i]
			switch b.Type {
			case ir.BlockImage:
				if b.Image == nil || b.Image.Data != "" || b.Image.URL == "" {
					continue
				}
				res, err := a.fetcher.FetchImage(ctx, b.Image.URL)
				if err != nil {
					return fmt.Errorf("materialize image: %w", err)
				}
				if res.Bytes > media.MaxAttachmentTotalBytes-total {
					return fmt.Errorf("%w: maximum %d bytes", media.ErrAttachmentBudgetExceeded, media.MaxAttachmentTotalBytes)
				}
				total += res.Bytes
				b.Image.Data = res.Data
				b.Image.MediaType = res.MediaType
				b.Image.URL = ""
			case ir.BlockToolResult:
				if err := walk(b.ToolResult); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for i := range req.Messages {
		if err := walk(req.Messages[i].Content); err != nil {
			return err
		}
	}
	return nil
}

// ensurePassthroughAttachments always decodes enough of a same-codec-id body to
// inventory and validate recognized attachments. The original body is still
// forwarded unchanged after validation; unknown fields are preserved by the
// passthrough path. A real decode error is returned so malformed attachment
// payloads cannot bypass limits via formatting.
func (p *Proxy) ensurePassthroughAttachments(ingress codec, body []byte, prep *attachmentPrep) error {
	if prep == nil || ingress.decodeRequest == nil {
		return nil
	}
	if prep.inspected {
		return nil
	}
	req, err := ingress.decodeRequest(body)
	if err != nil {
		return err
	}
	return prep.inspectDecoded(req)
}

func mediaTerminal(err error) attemptResult {
	return terminal(media.ClientErrorStatus(err), err.Error(), "invalid_request_error")
}

func incompatibleSkip(reason string) attemptResult {
	// retry=true so the loop advances, but callers must not penalizeProvider.
	return attemptResult{
		retry:   true,
		status:  400,
		errMsg:  clampErrorMessage("attachment not supported by upstream: " + reason),
		logErr:  "attachment_incompatible",
		errType: "invalid_request_error",
	}
}

// materializeSkip is a non-health failover for remote fetch / materialization
// failures. The target is not contacted further and is not penalized.
func materializeSkip(err error) attemptResult {
	return attemptResult{
		retry:   true,
		status:  media.ClientErrorStatus(err),
		errMsg:  clampErrorMessage(err.Error()),
		logErr:  skipLogAttachment,
		errType: "invalid_request_error",
	}
}

// skipLogAttachment distinguishes structural/materialization attachment skips
// from real upstream failures so serve does not penalize provider health.
const skipLogAttachment = "attachment_incompatible"
