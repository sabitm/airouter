package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
	"airouter/internal/proxy/thinking"
)

// streamPassthrough relays an upstream SSE response of the same protocol as the
// ingress, rewriting only the request model. Events are re-emitted (preserving
// names) so each is flushed to the client immediately. It returns an
// attemptResult so the resolution loop can fail over to the next target on a
// pre-commit failure; once the 200 header is written the response is committed.
func (p *Proxy) streamPassthrough(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, upstreamModel string, body []byte, clientHeaders http.Header) attemptResult {
	rewritten, err := finalizeRequestBody(body, upstreamModel, ingress, provider)
	if err != nil {
		return terminal(http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
	}
	resp, err := p.forwardStream(ctx, provider, ingress.upstreamPath, rewritten, clientHeaders, ingress.streamAccept)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorMax))
		return retryableUpstreamStatus(resp.StatusCode, errBody)
	}

	sw, ok := sse.NewWriter(w)
	if !ok {
		return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
	}
	// Streaming passthrough relays raw events unchanged; usage is sniffed out of
	// the relayed SSE without mutating it, so the log can record token counts.
	res.status = http.StatusOK
	w.WriteHeader(http.StatusOK)
	reader := sse.NewReader(resp.Body)
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			return committed()
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				observability.Logger(ctx, p.logger).Debug("client_disconnected",
					"event", "client_disconnected",
					"ingress", ingress.id,
				)
			} else {
				observability.Logger(ctx, p.logger).Error("stream_read_failed",
					"event", "stream_read_failed",
					"ingress", ingress.id,
					"error", err,
				)
			}
			return committed()
		}
		sniffStreamUsage(ev.Data, res)
		if err := sw.WriteEvent(ev.Name, ev.Data); err != nil {
			return committed() // client disconnected
		}
	}
}

// sniffStreamUsage extracts token counts from one raw SSE event's data,
// accepting both OpenAI (prompt_tokens/completion_tokens, top-level usage) and
// Anthropic (input_tokens/output_tokens, nested under message.usage at start or
// usage at message_delta) shapes. Each field is only overwritten when present
// and nonzero, so values reported on different events across the stream
// accumulate rather than reset.
func sniffStreamUsage(data []byte, res *reqResult) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	// Anthropic reports cached input under separate fields (cache_creation /
	// cache_read); fold them into the input total so cache state does not skew it.
	type usageFields struct {
		PromptTokens             int `json:"prompt_tokens"`
		CompletionTokens         int `json:"completion_tokens"`
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	}
	var u struct {
		Usage   *usageFields `json:"usage"`
		Message *struct {
			Usage *usageFields `json:"usage"`
		} `json:"message"`
		// Responses nests usage under response.usage on the response.completed event.
		Response *struct {
			Usage *usageFields `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &u) != nil {
		return
	}
	apply := func(f *usageFields) {
		if f == nil {
			return
		}
		if in := f.PromptTokens + f.InputTokens + f.CacheCreationInputTokens + f.CacheReadInputTokens; in != 0 {
			res.inTok = in
		}
		if out := f.CompletionTokens + f.OutputTokens; out != 0 {
			res.outTok = out
		}
	}
	apply(u.Usage)
	if u.Message != nil {
		apply(u.Message.Usage)
	}
	if u.Response != nil {
		apply(u.Response.Usage)
	}
}

// streamTranslated converts an ingress streaming request to the backend
// protocol, then pumps backend SSE events through the IR into ingress-format
// SSE. Pre-commit failures are retryable so the resolution loop can fail over;
// once the 200 header is written, errors mid-stream simply terminate.
func (p *Proxy) streamTranslated(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, body []byte) attemptResult {
	req, err := ingress.decodeRequest(body)
	if err != nil {
		return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	// Prefer Capture over typed decode so unfamiliar reasoning fields survive.
	if captured := thinking.Capture(body); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	req.Stream = true

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to encode upstream request", "api_error")
	}
	// Provider-aware reasoning finalization before protocol prepare/signing.
	upstreamBody, err = finalizeEncodedBody(upstreamBody, req, backend, provider)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to finalize upstream request", "api_error")
	}
	upstreamBody, err = prepareUpstreamRequest(ctx, backend, provider, upstreamBody)
	if err != nil {
		return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	resp, err := p.forwardStream(ctx, provider, backend.upstreamPath, upstreamBody, nil, backend.streamAccept)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorMax))
		return retryableUpstreamStatus(resp.StatusCode, errBody)
	}

	sw, ok := sse.NewWriter(w)
	if !ok {
		return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
	}
	res.status = http.StatusOK
	w.WriteHeader(http.StatusOK)

	enc := ingress.newStreamEncoder(upstreamModel)
	err = backend.decodeStream(resp.Body, func(ev ir.StreamEvent) error {
		// Token counts arrive on distinct events depending on backend: Anthropic
		// reports input at message start, OpenAI reports both at finish. Take
		// input from whichever event carries a nonzero value.
		switch ev.Kind {
		case ir.EventMessageStart:
			if ev.InputTokens != 0 {
				res.inTok = ev.InputTokens
			}
		case ir.EventFinish:
			if ev.InputTokens != 0 {
				res.inTok = ev.InputTokens
			}
			res.outTok = ev.OutputTokens
		}
		return enc.Encode(ev, sw)
	})
	if err != nil {
		// A canceled context means the client disconnected after receiving the
		// response; that is routine, not a server error, so do not log it as one.
		if errors.Is(err, context.Canceled) {
			observability.Logger(ctx, p.logger).Debug("client_disconnected",
				"event", "client_disconnected",
				"ingress", ingress.id,
				"backend", backend.id,
			)
			return committed()
		}
		// Already streaming; cannot switch to a unary error. Stop cleanly.
		observability.Logger(ctx, p.logger).Error("stream_decode_failed",
			"event", "stream_decode_failed",
			"ingress", ingress.id,
			"backend", backend.id,
			"error", "upstream stream decode failed",
		)
		return committed()
	}
	if err := enc.Close(sw); err != nil {
		observability.Logger(ctx, p.logger).Error("stream_encode_close_failed",
			"event", "stream_encode_close_failed",
			"ingress", ingress.id,
			"backend", backend.id,
			"error", err,
		)
	}
	return committed()
}

// rewriteModel replaces the top-level "model" field, preserving all other fields.
func rewriteModel(body []byte, model string) ([]byte, error) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return nil, err
	}
	generic["model"], _ = json.Marshal(model)
	return json.Marshal(generic)
}

// applyUpstreamModel sets the backend model from a combo target, applying a
// model(level) thinking suffix override when present (suffix wins over body).
// Intensity only; outgoing dialect is applied later by the provider-aware finalizer.
func applyUpstreamModel(req *ir.Request, upstreamModel string) {
	base, override := thinking.ParseSuffix(upstreamModel)
	req.Model = base
	if override != nil {
		req.Thinking = thinking.ToIR(thinking.Merge(thinking.FromIR(req.Thinking), override))
	}
}

// finalizeRequestBody is the shared passthrough finalizer: suffix > body intent >
// required default > model-only rewrite. Uses the target provider's dialect.
func finalizeRequestBody(body []byte, upstreamModel string, ingress codec, provider *domain.Provider) ([]byte, error) {
	dialect := domain.ReasoningNone
	proto := domain.ProtocolOpenAI
	if provider != nil {
		dialect = provider.Reasoning()
		proto = provider.Protocol
	}
	// Passthrough shares ingress transport with the backend.
	if proto == "" {
		proto = protocolForCodec(ingress)
	}
	return thinking.FinalizeBody(body, upstreamModel, ingress.id, proto, dialect)
}

// finalizeEncodedBody normalizes reasoning on an already-encoded backend body
// using the target provider dialect. Runs before prepareUpstreamRequest so
// WAF/signing/cloak see the final bytes. Special protocol-managed dialects
// (none/cursor/kiro/...) leave the body unchanged aside from model already set.
func finalizeEncodedBody(body []byte, req *ir.Request, backend codec, provider *domain.Provider) ([]byte, error) {
	if provider == nil {
		return body, nil
	}
	dialect := provider.Reasoning()
	// Protocol-managed: codecs own their shape; do not run the generic writer.
	switch provider.Protocol {
	case domain.ProtocolKiro, domain.ProtocolQoder, domain.ProtocolAntigravity, domain.ProtocolCursor:
		return body, nil
	}
	// Explicit none: strip any transport-default reasoning the encoder may have
	// written so the generic writer stays disabled.
	if dialect == domain.ReasoningNone {
		return thinking.ApplyWire(backend.id, body, req.Model, nil, provider.Protocol, dialect)
	}
	// Codex keeps the encoder's required default and native hyphen suffix when no
	// unified body/suffix intent exists. Explicit intent is selectively patched.
	cfg := thinking.FromIR(req.Thinking)
	if provider.Protocol == domain.ProtocolOpenAICodex && cfg == nil {
		return body, nil
	}
	caps := thinking.CapsFor(req.Model, provider.Protocol, dialect)
	eff := thinking.ResolveIntent(cfg, nil, caps)
	if eff == nil && caps.RequiredDefault == "" {
		return body, nil
	}
	out, err := thinking.ApplyWire(backend.id, body, req.Model, eff, provider.Protocol, dialect)
	if err != nil {
		return nil, err
	}
	if provider.Protocol == domain.ProtocolOpenAICodex {
		out = responses.SyncCodexReasoningInclude(out)
	}
	return out, nil
}

// protocolForCodec maps a codec id to a domain protocol for caps resolution.
func protocolForCodec(c codec) domain.Protocol {
	switch c.id {
	case "oai-chat":
		return domain.ProtocolOpenAI
	case "anth-msg":
		return domain.ProtocolAnthropic
	case "oai-responses":
		return domain.ProtocolOpenAIResponses
	case "oai-codex":
		return domain.ProtocolOpenAICodex
	case "claude-code":
		return domain.ProtocolClaudeCode
	case "cursor":
		return domain.ProtocolCursor
	case "kiro":
		return domain.ProtocolKiro
	case "qoder":
		return domain.ProtocolQoder
	case "antigravity":
		return domain.ProtocolAntigravity
	default:
		return domain.ProtocolOpenAI
	}
}

// rewriteModelWithThinking is retained for tests; delegates to FinalizeBody with
// OpenAI protocol defaults when no provider is in scope.
func rewriteModelWithThinking(body []byte, upstreamModel, formatID string) ([]byte, error) {
	proto := domain.ProtocolOpenAI
	dialect := domain.ReasoningOpenAI
	switch formatID {
	case "anth-msg":
		proto = domain.ProtocolAnthropic
		dialect = domain.ReasoningClaude
	case "oai-responses":
		proto = domain.ProtocolOpenAIResponses
	case "oai-codex":
		proto = domain.ProtocolOpenAICodex
		dialect = domain.ReasoningCodex
	}
	return thinking.FinalizeBody(body, upstreamModel, formatID, proto, dialect)
}
