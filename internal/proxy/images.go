package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/images"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
)

type imageCodec struct {
	decodeRequest  func([]byte) (*ir.ImageGenerationRequest, error)
	encodeRequest  func(*ir.ImageGenerationRequest) ([]byte, error)
	decodeResponse func([]byte) (*ir.ImageGenerationResponse, error)
	encodeResponse func(*ir.ImageGenerationResponse) ([]byte, error)
}

var imagesCodec = codec{
	id: "oai-images", encodeError: openai.EncodeError,
	image: &imageCodec{decodeRequest: images.DecodeRequest, encodeResponse: images.EncodeResponse},
}

var codexImagesCodec = codec{
	id: "oai-codex-images", protocol: domain.ProtocolOpenAICodex,
	upstreamPath: "/images/generations", encodeError: openai.EncodeError,
	image: &imageCodec{encodeRequest: images.EncodeCodexRequest, decodeResponse: images.DecodeCodexResponse},
}

func (p *Proxy) imageTargets(ctx context.Context, combo *domain.Combo) []domain.ComboTarget {
	var compatible []domain.ComboTarget
	for _, target := range activeComboTargets(combo) {
		if target.Provider == nil || target.Provider.Protocol != domain.ProtocolOpenAICodex || strings.TrimSpace(target.UpstreamModel) == "" {
			observability.Logger(ctx, p.logger).Debug("image_target_skipped",
				"event", "image_target_skipped", "combo", combo.Name,
				"provider_id", target.ProviderID, "reason", "target does not support native image generation")
			continue
		}
		compatible = append(compatible, target)
	}
	return p.orderCompatibleTargets(combo, compatible)
}

func (p *Proxy) serveImages(w http.ResponseWriter, r *http.Request, res *reqResult, rec *domain.RequestLog, ingress codec, combo *domain.Combo, body []byte) {
	req, err := ingress.image.decodeRequest(body)
	if err != nil {
		res.fail(w, ingress, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !strings.EqualFold(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]), "application/json") {
		res.fail(w, ingress, http.StatusBadRequest, "images: only application/json is supported", "invalid_request_error")
		return
	}
	targets := p.imageTargets(r.Context(), combo)
	if len(targets) == 0 {
		if len(activeComboTargets(combo)) == 0 {
			res.fail(w, ingress, http.StatusInternalServerError, "combo has no targets", "api_error")
		} else {
			res.fail(w, ingress, http.StatusBadRequest, "images: combo has no compatible image-generation targets", "invalid_request_error")
		}
		return
	}
	// Images has request-local identity, not the Responses prompt-cache identity.
	identity := newUUID()
	headers := http.Header{"Accept": {"application/json"}, "X-Codex-Image-Turn-Id": {identity}}
	if trace := traceInfoFrom(r.Context()); trace != nil {
		trace.CodexSessionID = identity
	}
	last := callerCanceledResult()
	for i, target := range targets {
		if ctxErr(r.Context()) {
			last = callerCanceledResult()
			break
		}
		rec.Provider = target.Provider.Name
		rec.UpstreamModel = target.UpstreamModel
		started := time.Now()
		attemptReq := *req
		attemptReq.Model = target.UpstreamModel
		last = p.attemptImage(w, r.Context(), res, ingress, target.Provider, &attemptReq, headers)
		if ctxErr(r.Context()) && !last.written {
			last = callerCanceledResult()
			break
		}
		if last.written {
			p.clearBackoff(target.ProviderID)
			res.inTok, res.outTok = last.inTok, last.outTok
			return
		}
		if !last.retry {
			break
		}
		p.penalizeProvider(target.ProviderID)
		willRetry := i < len(targets)-1
		p.logUpstreamAttemptFailed(r.Context(), combo.Name, target.Provider, target.UpstreamModel, codexImagesCodec, i+1, last, willRetry)
		if willRetry {
			p.recordLog(r.Context(), &domain.RequestLog{
				AccessKeyName: rec.AccessKeyName, Combo: combo.Name, Provider: target.Provider.Name,
				UpstreamModel: target.UpstreamModel, Format: ingress.id, Status: last.status,
				InputTokens: last.inTok, OutputTokens: last.outTok,
				LatencyMS: time.Since(started).Milliseconds(), ErrMsg: last.errMsg,
			})
		}
	}
	status := last.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	res.fail(w, ingress, status, last.errMsg, last.errType)
	res.logErr = last.logErr
	res.inTok, res.outTok = last.inTok, last.outTok
}

func (p *Proxy) attemptImage(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, req *ir.ImageGenerationRequest, headers http.Header) attemptResult {
	backend := codexImagesCodec
	body, err := backend.image.encodeRequest(req)
	if err != nil {
		return terminalEncodeError(err)
	}
	status, raw, err := p.forward(ctx, provider, backend.upstreamPath, body, headers)
	if err != nil {
		return retryable(http.StatusBadGateway, "image upstream request failed", "api_error")
	}
	if status < 200 || status >= 300 {
		return retryableUpstreamStatus(status, raw)
	}
	resp, decodeErr := backend.image.decodeResponse(raw)
	in, out := 0, 0
	if resp != nil && resp.Usage != nil {
		in, out = resp.Usage.InputTokens, resp.Usage.OutputTokens
	}
	if decodeErr != nil {
		return retryable(http.StatusBadGateway, "image upstream returned an invalid image response", "api_error").withTokens(in, out)
	}
	encoded, err := ingress.image.encodeResponse(resp)
	if err != nil {
		return retryable(http.StatusBadGateway, "failed to encode image response", "api_error").withTokens(in, out)
	}
	if ctxErr(ctx) {
		return callerCanceledResult()
	}
	res.status = http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
	return committed().withTokens(in, out)
}
