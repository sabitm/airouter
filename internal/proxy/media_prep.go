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
		errMsg:  "attachment not supported by upstream: " + reason,
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
		errMsg:  err.Error(),
		logErr:  skipLogAttachment,
		errType: "invalid_request_error",
	}
}

// skipLogAttachment distinguishes structural/materialization attachment skips
// from real upstream failures so serve does not penalize provider health.
const skipLogAttachment = "attachment_incompatible"
