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
func (p *Proxy) streamPassthrough(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, upstreamModel string, body []byte, clientHeaders http.Header, prep *attachmentPrep) attemptResult {
	if err := p.ensurePassthroughAttachments(ingress, body, prep); err != nil {
		return mediaTerminal(err)
	}
	if reason := prep.checkCompatible(ingress, false); reason != "" {
		return incompatibleSkip(reason)
	}
	rewritten, err := finalizeRequestBody(body, upstreamModel, ingress, provider)
	if err != nil {
		return terminal(http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
	}
	// OpenAI Chat streaming omits a terminal usage chunk unless asked; force it
	// on same-id passthrough so logs and clients still see authoritative counts.
	if ingress.id == "oai-chat" {
		rewritten, err = forceOpenAIStreamIncludeUsage(rewritten)
		if err != nil {
			return terminal(http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		}
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

	// Check flusher support without writing headers so a pre-event failure can
	// still fail over. Headers are set only when the first event is relayed.
	if _, ok := w.(http.Flusher); !ok {
		return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
	}
	var sw *sse.Writer
	committedOut := false
	reader := sse.NewReader(resp.Body)
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			if !committedOut {
				return retryable(http.StatusBadGateway, "upstream returned an empty stream", "api_error")
			}
			return committed()
		}
		if err != nil {
			// Client disconnects are checked before the commit gate: retrying other
			// targets for a gone client only wastes upstream calls.
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				observability.Logger(ctx, p.logger).Debug("client_disconnected",
					"event", "client_disconnected",
					"ingress", ingress.id,
				)
			} else if !committedOut {
				return retryable(http.StatusBadGateway, "upstream stream read failed", "api_error")
			} else {
				observability.Logger(ctx, p.logger).Error("stream_read_failed",
					"event", "stream_read_failed",
					"ingress", ingress.id,
					"error", err,
				)
			}
			return committed()
		}
		if !committedOut {
			var ok bool
			sw, ok = sse.NewWriter(w)
			if !ok {
				return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
			}
			// Streaming passthrough relays raw events unchanged; usage is sniffed out of
			// the relayed SSE without mutating it, so the log can record token counts.
			res.status = http.StatusOK
			w.WriteHeader(http.StatusOK)
			committedOut = true
		}
		sniffStreamUsage(ev.Data, res, ingress.id)
		if err := sw.WriteEvent(ev.Name, ev.Data); err != nil {
			return committed() // client disconnected
		}
	}
}

// sniffStreamUsage extracts token counts from one raw SSE event's data without
// mutating the relayed bytes. OpenAI nests usage top-level; Anthropic under
// message.usage / message_delta usage; Responses under response.usage. Each
// field is only overwritten when the chosen family yields a nonzero total, so
// values reported on different events across the stream accumulate rather than
// reset. codecID selects one alias family so hybrid objects are not double-counted.
func sniffStreamUsage(data []byte, res *reqResult, codecID string) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var u struct {
		Usage   json.RawMessage `json:"usage"`
		Message *struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Response *struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &u) != nil {
		return
	}
	apply := func(raw json.RawMessage) {
		if len(raw) == 0 {
			return
		}
		in, out := parseUsageObject(raw, codecID)
		if in != 0 {
			res.inTok = in
		}
		if out != 0 {
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

// forceOpenAIStreamIncludeUsage sets stream_options.include_usage=true on an
// OpenAI Chat Completions body so upstream emits a terminal usage chunk. Other
// stream_options keys are preserved; include_usage=false is overridden. No-op
// when stream is not true.
func forceOpenAIStreamIncludeUsage(body []byte) ([]byte, error) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return nil, err
	}
	var stream bool
	if raw, ok := generic["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	if !stream {
		return body, nil
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := generic["stream_options"]; ok && len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &opts)
	}
	opts["include_usage"], _ = json.Marshal(true)
	generic["stream_options"], _ = json.Marshal(opts)
	return json.Marshal(generic)
}

// streamTranslated converts an ingress streaming request to the backend
// protocol, then pumps backend SSE events through the IR into ingress-format
// SSE. Pre-commit failures are retryable so the resolution loop can fail over;
// once the 200 header is written, errors mid-stream simply terminate.
func (p *Proxy) streamTranslated(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, body []byte, prep *attachmentPrep) attemptResult {
	req, err := ingress.decodeRequest(body)
	if err != nil {
		return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	if err := prep.inspectDecoded(req); err != nil {
		return mediaTerminal(err)
	}
	if reason := prep.checkCompatible(backend, true); reason != "" {
		return incompatibleSkip(reason)
	}
	if err := prep.materialize(ctx, req, backend); err != nil {
		return materializeSkip(err)
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

	// Check flusher support without writing headers so pre-commit decode failures
	// can still fail over to the next target.
	if _, ok := w.(http.Flusher); !ok {
		return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
	}

	enc := ingress.newStreamEncoder(upstreamModel)
	sink := &translatedSink{w: w, res: res, enc: enc}
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
		return sink.handle(ev)
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
		if !sink.committed {
			if _, ok := ir.AsStreamFailure(err); ok {
				return retryableStreamFailure(err)
			}
			return retryableStreamDecode(err)
		}
		// Post-commit: emit an ingress error frame and record the failure. Close is
		// skipped so no finish_reason/usage/[DONE] follows; the error frame is terminal.
		errMsg := "upstream stream decode failed"
		errType := "api_error"
		if sf, ok := ir.AsStreamFailure(err); ok {
			if sf.Message != "" {
				errMsg = sf.Message
			}
			if sf.Type != "" {
				errType = sf.Type
			}
		} else if err.Error() != "" {
			errMsg = err.Error()
		}
		observability.Logger(ctx, p.logger).Error("stream_decode_failed",
			"event", "stream_decode_failed",
			"ingress", ingress.id,
			"backend", backend.id,
			"error", "upstream stream decode failed",
		)
		if sink.sw != nil {
			_ = sink.enc.EncodeError(sink.sw, errMsg, errType)
		}
		res.errMsg = errMsg
		res.logErr = "upstream stream decode failed"
		return committedFailure(http.StatusOK, errMsg, "upstream stream decode failed")
	}
	if !sink.committed {
		return retryable(http.StatusBadGateway, "upstream returned an empty stream", "api_error")
	}
	// Clean decode with a committed stream: Close emits format trailers ([DONE],
	// etc.). Skipped on post-commit failure so the error frame stays terminal.
	if err := enc.Close(sink.sw); err != nil {
		observability.Logger(ctx, p.logger).Error("stream_encode_close_failed",
			"event", "stream_encode_close_failed",
			"ingress", ingress.id,
			"backend", backend.id,
			"error", err,
		)
	}
	return committed()
}

// translatedSink buffers lifecycle preamble until the first client-visible byte,
// then commits SSE headers and replays. Commitment is deferred past
// EventMessageStart so an upstream error before real output can still fail over.
type translatedSink struct {
	w         http.ResponseWriter
	res       *reqResult
	enc       streamEncoder
	sw        *sse.Writer
	pending   []ir.StreamEvent
	committed bool
}

func (s *translatedSink) handle(ev ir.StreamEvent) error {
	if !s.committed {
		if ev.Kind == ir.EventMessageStart {
			s.pending = append(s.pending, ev)
			return nil
		}
		switch ev.Kind {
		case ir.EventTextDelta, ir.EventToolCallStart, ir.EventToolCallDelta, ir.EventFinish:
			if err := s.commit(); err != nil {
				return err
			}
		default:
			s.pending = append(s.pending, ev)
			return nil
		}
	}
	return s.enc.Encode(ev, s.sw)
}

func (s *translatedSink) commit() error {
	if s.committed {
		return nil
	}
	sw, ok := sse.NewWriter(s.w)
	if !ok {
		return errors.New("streaming unsupported by server")
	}
	s.sw = sw
	s.res.status = http.StatusOK
	s.w.WriteHeader(http.StatusOK)
	s.committed = true
	for _, ev := range s.pending {
		if err := s.enc.Encode(ev, s.sw); err != nil {
			return err
		}
	}
	s.pending = nil
	return nil
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
