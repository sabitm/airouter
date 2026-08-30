package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
	"airouter/internal/proxy/thinking"
)

const (
	passthroughPendingMaxBytes  = 1 << 20
	passthroughPendingMaxEvents = 1024
)

// streamPassthrough relays an upstream SSE response of the same codec as the
// ingress, rewriting only the request model. Lifecycle-only events are buffered
// until visible output or a successful terminal so an explicit protocol error
// can still fail over. Once the 200 header is written, native events (including
// a later error frame) are relayed unchanged and failover is forbidden.
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
	// still fail over. Headers are set only when the first visible/terminal event is relayed.
	if _, ok := w.(http.Flusher); !ok {
		return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
	}
	var sw *sse.Writer
	committedOut := false
	var pending []sse.Event
	pendingBytes := 0
	reader := sse.NewReader(resp.Body)

	commit := func() attemptResult {
		if committedOut {
			return attemptResult{}
		}
		var ok bool
		sw, ok = sse.NewWriter(w)
		if !ok {
			return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
		}
		// Usage is sniffed only from events that are actually relayed so a failed
		// target's counts cannot contaminate a later winning attempt.
		res.status = http.StatusOK
		w.WriteHeader(http.StatusOK)
		committedOut = true
		for _, ev := range pending {
			sniffStreamUsage(ev.Data, res, ingress.id)
			if err := sw.WriteEvent(ev.Name, ev.Data); err != nil {
				return committed()
			}
		}
		pending = nil
		pendingBytes = 0
		return attemptResult{}
	}

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
				return committed()
			}
			if !committedOut {
				return retryable(http.StatusBadGateway, "upstream stream read failed", "api_error")
			}
			observability.Logger(ctx, p.logger).Error("stream_read_failed",
				"event", "stream_read_failed",
				"ingress", ingress.id,
				"error", err,
			)
			const readFail = "upstream stream read failed"
			res.errMsg = readFail
			res.logErr = readFail
			return committedFailure(http.StatusOK, readFail, readFail)
		}

		class, fail := classifyPassthroughEvent(ingress.id, ev)
		if !committedOut {
			switch class {
			case passLifecycle:
				eventBytes := len(ev.Name) + len(ev.Data)
				if len(pending) < passthroughPendingMaxEvents && eventBytes <= passthroughPendingMaxBytes-pendingBytes {
					pending = append(pending, ev)
					pendingBytes += eventBytes
					continue
				}
				if ar := commit(); ar.written || ar.status != 0 {
					return ar
				}
			case passFailure:
				return retryableStreamFailure(fail)
			}
			if !committedOut {
				if ar := commit(); ar.written || ar.status != 0 {
					return ar
				}
			}
		}

		if class == passFailure {
			sniffStreamUsage(ev.Data, res, ingress.id)
			if err := sw.WriteEvent(ev.Name, ev.Data); err != nil {
				return committed()
			}
			errMsg := "upstream stream failed"
			if fail != nil && fail.Message != "" {
				errMsg = fail.Message
			}
			res.errMsg = errMsg
			res.logErr = "upstream stream failed"
			return committedFailure(http.StatusOK, errMsg, "upstream stream failed")
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
	var writeFrame func([]byte) error
	closeWrite := func() {}
	var resp *http.Response
	if backend.decodeStreamDuplex != nil {
		resp, writeFrame, closeWrite, err = p.forwardStreamDuplex(ctx, provider, backend.upstreamPath, upstreamBody, nil, backend.streamAccept)
	} else {
		resp, err = p.forwardStream(ctx, provider, backend.upstreamPath, upstreamBody, nil, backend.streamAccept)
	}
	defer closeWrite()
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
	var inTok, outTok int
	publishUsage := func() {
		res.inTok = inTok
		res.outTok = outTok
	}
	decode := backend.decodeStream
	if backend.decodeStreamDuplex != nil {
		decode = func(r io.Reader, emit func(ir.StreamEvent) error) error {
			if backend.protocol == domain.ProtocolCursor {
				return cursor.DecodeAgentStreamTools(req.Tools, r, writeFrame, emit)
			}
			return backend.decodeStreamDuplex(r, writeFrame, emit)
		}
	}
	emit := func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EventMessageStart:
			if ev.InputTokens != 0 {
				inTok = ev.InputTokens
			}
		case ir.EventFinish:
			if ev.InputTokens != 0 {
				inTok = ev.InputTokens
			}
			outTok = ev.OutputTokens
		}
		err := sink.handle(ev)
		if sink.committed {
			publishUsage()
		}
		return err
	}
	err = decode(resp.Body, emit)
	if ub, ok := cursor.AsUnmatchedBuiltin(err); ok && len(req.Tools) > 0 {
		closeWrite()
		_ = resp.Body.Close()
		retryReq := cursor.WithBuiltinRejection(req, ub.Name)
		retryBody, rerr := backend.encodeRequest(retryReq)
		if rerr == nil {
			retryBody, rerr = finalizeEncodedBody(retryBody, retryReq, backend, provider)
		}
		if rerr == nil {
			retryBody, rerr = prepareUpstreamRequest(ctx, backend, provider, retryBody)
		}
		var retryResp *http.Response
		var retryWrite func([]byte) error
		var retryClose func()
		if rerr == nil {
			retryResp, retryWrite, retryClose, rerr = p.forwardStreamDuplex(ctx, provider, backend.upstreamPath, retryBody, nil, backend.streamAccept)
		}
		if rerr == nil {
			defer retryClose()
			defer retryResp.Body.Close()
			if retryResp.StatusCode >= 200 && retryResp.StatusCode < 300 {
				err = cursor.DecodeAgentStreamTools(req.Tools, retryResp.Body, retryWrite, emit)
				if _, again := cursor.AsUnmatchedBuiltin(err); again {
					err = emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn})
				}
			} else {
				err = emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn})
			}
		} else {
			err = emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn})
		}
	}
	if err != nil {
		// A canceled context means the client disconnected after receiving the
		// response; that is routine, not a server error, so do not log it as one.
		// Duplex streams surface cancel as a body-closed read error (the HTTP/2
		// client cannot cancel a read while the request body is open), so key on
		// ctx rather than the error value alone.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			observability.Logger(ctx, p.logger).Debug("client_disconnected",
				"event", "client_disconnected",
				"ingress", ingress.id,
				"backend", backend.id,
			)
			if sink.committed {
				publishUsage()
			}
			return committed().withTokens(inTok, outTok)
		}
		if !sink.committed {
			if _, ok := ir.AsStreamFailure(err); ok {
				return retryableStreamFailure(err).withTokens(inTok, outTok)
			}
			return retryableStreamDecode(err).withTokens(inTok, outTok)
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
		publishUsage()
		return committedFailure(http.StatusOK, errMsg, "upstream stream decode failed").withTokens(inTok, outTok)
	}
	if !sink.committed {
		return retryable(http.StatusBadGateway, "upstream returned an empty stream", "api_error").withTokens(inTok, outTok)
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
	publishUsage()
	return committed().withTokens(inTok, outTok)
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
