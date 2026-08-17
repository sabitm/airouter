package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/media"
	"airouter/internal/proxy/thinking"
	"airouter/internal/store"
)

const maxBodyBytes = 16 << 20 // 16 MiB ceiling on inbound request bodies

// upstreamErrorMax caps how many bytes of an upstream error body are read for
// extracting a client-facing message and persisted request-history detail. HAR
// capture is independently bounded by harlog.MaxBody.
const upstreamErrorMax = 1 << 20 // 1 MiB

// reqResult accumulates the outcome of one request for logging. Each serve path
// fills it in; serve records a RequestLog once on completion.
type reqResult struct {
	status int
	inTok  int
	outTok int
	errMsg string
	logErr string
}

// fail writes an error envelope and records the failure on the result.
func (res *reqResult) fail(w http.ResponseWriter, ingress codec, status int, message, errType string) {
	res.status = status
	res.errMsg = message
	res.logErr = message
	writeErr(w, ingress, status, message, errType)
}

// attemptResult reports the outcome of trying one combo target.
//
//   - written:  a response was committed to the client (success, or a stream
//     that began). The resolution loop must not write anything further and must
//     not attempt another target.
//   - failed:   when written, marks a committed failure (partial stream then
//     error). Stops failover like written, but must not clear provider backoff
//     and must record errMsg on the request log.
//   - retry:    the attempt failed before committing; the loop may try the next
//     target. status/errMsg/errType describe the failure for a possible envelope.
//
// A terminal pre-commit error (bad request body, encode failure) returns neither
// written nor retry: the loop stops and surfaces the envelope once.
type attemptResult struct {
	written bool
	failed  bool
	retry   bool
	status  int
	errMsg  string
	logErr  string
	errType string
	// inTok/outTok are this attempt's usage only. They must not be read from
	// the shared reqResult: a pre-commit failure can decode usage and then
	// fail over, and that count belongs on this row alone.
	inTok  int
	outTok int
	hasTok bool
}

func (ar attemptResult) withTokens(in, out int) attemptResult {
	ar.inTok = in
	ar.outTok = out
	ar.hasTok = true
	return ar
}

func committed() attemptResult { return attemptResult{written: true} }

// committedFailure stops failover after bytes were written, keeps the provider
// penalized, and surfaces errMsg in request history. status is the HTTP status
// already committed (typically 200 for SSE).
func committedFailure(status int, errMsg, logErr string) attemptResult {
	if logErr == "" {
		logErr = errMsg
	}
	return attemptResult{written: true, failed: true, status: status, errMsg: errMsg, logErr: logErr, errType: "api_error"}
}

func retryable(status int, message, errType string) attemptResult {
	return attemptResult{retry: true, status: status, errMsg: message, logErr: message, errType: errType}
}

// retryableUpstreamStatus preserves the upstream message for the client and
// persisted request history while keeping raw non-JSON response bodies out of
// DEBUG terminal logs.
func retryableUpstreamStatus(status int, body []byte) attemptResult {
	return attemptResult{
		retry:   true,
		status:  status,
		errMsg:  upstreamErrorMessage(body),
		logErr:  http.StatusText(status),
		errType: "api_error",
	}
}

// retryableStreamDecode preserves protocol error detail for the client and
// request history while keeping decoder payload snippets out of terminal logs.
func retryableStreamDecode(err error) attemptResult {
	return attemptResult{
		retry:   true,
		status:  http.StatusBadGateway,
		errMsg:  "upstream stream decode failed: " + err.Error(),
		logErr:  "upstream stream decode failed",
		errType: "api_error",
	}
}

// retryableStreamFailure maps a structured upstream stream failure to a
// pre-commit failover result. Client/history get the upstream message; terminal
// logs get a generic string only.
func retryableStreamFailure(err error) attemptResult {
	status := http.StatusBadGateway
	errMsg := "upstream stream failed"
	if sf, ok := ir.AsStreamFailure(err); ok {
		if sf.Message != "" {
			errMsg = sf.Message
		}
		if isServiceUnavailableFailure(sf) {
			status = http.StatusServiceUnavailable
		}
	} else if err != nil && err.Error() != "" {
		errMsg = err.Error()
	}
	return attemptResult{
		retry:   true,
		status:  status,
		errMsg:  errMsg,
		logErr:  "upstream stream failed",
		errType: "api_error",
	}
}

func isServiceUnavailableFailure(sf *ir.StreamFailure) bool {
	if sf == nil {
		return false
	}
	if sf.Type == "service_unavailable_error" {
		return true
	}
	switch strings.ToLower(sf.Code) {
	case "server_is_overloaded", "overloaded", "service_unavailable":
		return true
	}
	return strings.Contains(strings.ToLower(sf.Message), "overloaded")
}

func terminal(status int, message, errType string) attemptResult {
	return attemptResult{status: status, errMsg: message, logErr: message, errType: errType}
}

// serve runs the full ingress lifecycle for one request. ingress is the codec
// for the endpoint the client called.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, ingress codec) {
	start := time.Now()
	res := &reqResult{status: http.StatusOK}
	rec := &domain.RequestLog{Format: ingress.id}
	defer func() {
		rec.Status = res.status
		rec.InputTokens = res.inTok
		rec.OutputTokens = res.outTok
		rec.ErrMsg = res.errMsg
		rec.LatencyMS = time.Since(start).Milliseconds()
		// The final client-facing outcome is distinct from any failed upstream
		// attempt: it is emitted only when the request itself ultimately failed.
		if res.errMsg != "" {
			observability.Logger(r.Context(), p.logger).Debug("request_failed",
				"event", "request_failed",
				"status", res.status,
				"error", res.logErr,
				"ingress", ingress.id,
				"method", r.Method,
			)
		}
		p.recordLog(r.Context(), rec)
	}()

	keyName, ok := p.authenticate(r)
	if !ok {
		res.fail(w, ingress, http.StatusUnauthorized, "invalid or missing access key", "authentication_error")
		return
	}
	rec.AccessKeyName = keyName

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			res.fail(w, ingress, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
			return
		}
		res.fail(w, ingress, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}

	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		res.fail(w, ingress, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	rec.Combo = meta.Model
	rec.Stream = meta.Stream
	if meta.Model == "" {
		res.fail(w, ingress, http.StatusBadRequest, "missing 'model' field", "invalid_request_error")
		return
	}

	combo, err := p.store.GetComboByName(r.Context(), meta.Model)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			res.fail(w, ingress, http.StatusNotFound, "unknown model (combo): "+meta.Model, "invalid_request_error")
			return
		}
		res.fail(w, ingress, http.StatusInternalServerError, "combo lookup failed", "api_error")
		return
	}
	// Inventory recognized attachments once before health ordering. Invalid
	// media is a client error with zero upstream contact. Structural transport
	// incompatibility filters targets before backoff skip credits are consumed.
	var prep attachmentPrep
	if ingress.decodeRequest != nil {
		req, err := ingress.decodeRequest(body)
		if err != nil {
			res.fail(w, ingress, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := prep.inspectDecoded(req); err != nil {
			res.fail(w, ingress, media.ClientErrorStatus(err), err.Error(), "invalid_request_error")
			return
		}
	}

	candidates, filterReason := p.orderTargets(r.Context(), combo, ingress, &prep)
	if len(candidates) == 0 {
		if prep.hasAttachments() {
			msg := "attachment not supported by upstream: no compatible provider in combo"
			if filterReason != "" {
				msg = "attachment not supported by upstream: " + filterReason
			}
			res.fail(w, ingress, http.StatusBadRequest, msg, "invalid_request_error")
			return
		}
		res.fail(w, ingress, http.StatusInternalServerError, "combo has no targets: "+meta.Model, "api_error")
		return
	}

	// Walk the ordered (attachment-compatible) targets. A target that fails before
	// any byte reaches the client falls through to the next; the first that commits
	// a response ends the walk. If all fail, the last failure's envelope is written
	// below. Materialization skips do not penalize provider health. A real upstream
	// failure on a compatible target is never hidden behind a later materialize skip.
	var last attemptResult
	var sawRealAttempt bool
	var lastReal attemptResult
	for i, t := range candidates {
		provider := t.Provider
		rec.Provider = provider.Name
		rec.UpstreamModel = t.UpstreamModel
		backend := backendCodec(provider.Protocol)

		attemptStart := time.Now()
		if ingress.id == backend.id {
			if meta.Stream {
				last = p.streamPassthrough(w, r.Context(), res, ingress, provider, t.UpstreamModel, body, r.Header, &prep)
			} else {
				last = p.servePassthrough(w, r.Context(), res, ingress, provider, t.UpstreamModel, body, r.Header, &prep)
			}
		} else if meta.Stream {
			last = p.streamTranslated(w, r.Context(), res, ingress, backend, provider, t.UpstreamModel, body, &prep)
		} else {
			last = p.serveTranslated(w, r.Context(), res, ingress, backend, provider, t.UpstreamModel, body, &prep)
		}

		attachmentSkip := last.retry && last.logErr == skipLogAttachment
		if !attachmentSkip && (last.retry || last.written || last.status != 0) {
			sawRealAttempt = true
			if last.retry || (!last.retry && !last.written) {
				lastReal = last
			}
		}

		if !last.retry {
			// A committed success clears the provider's penalty so it is immediately
			// eligible again. A committed failure keeps the penalty (bytes went out
			// so failover is impossible, but the provider was unhealthy). A terminal
			// pre-commit error leaves backoff untouched: not an upstream-health signal.
			if last.written && !last.failed {
				p.clearBackoff(provider.ID)
			}
			if last.written && last.failed {
				res.status = last.status
				if res.status == 0 {
					res.status = http.StatusOK
				}
				res.errMsg = last.errMsg
				res.logErr = last.logErr
			}
			break
		}
		if attachmentSkip {
			// Materialization / late structural skip: no health penalty.
			observability.Logger(r.Context(), p.logger).Debug("attachment_target_skipped",
				"event", "attachment_target_skipped",
				"combo", combo.Name,
				"provider", provider.Name,
				"upstream_model", t.UpstreamModel,
				"protocol", string(provider.Protocol),
				"format", backend.id,
				"reason", last.errMsg,
			)
			continue
		}
		// The attempt failed over before committing bytes: penalize the provider so
		// subsequent requests defer it behind healthy targets. Applies to the last
		// target too, so a whole-combo outage still grows the window.
		p.penalizeProvider(provider.ID)
		willRetry := i < len(candidates)-1
		p.logUpstreamAttemptFailed(r.Context(), combo.Name, provider, t.UpstreamModel, backend, i+1, last, willRetry)
		if willRetry {
			// Persist a row per failed-and-superseded target so the Logs tab shows
			// which provider errored before failover advanced. The final outcome
			// (a later success or the last target's failure) is still covered by the
			// deferred rec log, so this only fires for intermediate failures.
			p.recordLog(r.Context(), &domain.RequestLog{
				AccessKeyName: rec.AccessKeyName,
				Combo:         rec.Combo,
				Provider:      provider.Name,
				UpstreamModel: t.UpstreamModel,
				Format:        ingress.id,
				Stream:        meta.Stream,
				Status:        last.status,
				InputTokens:   last.inTok,
				OutputTokens:  last.outTok,
				ErrMsg:        last.errMsg,
				LatencyMS:     time.Since(attemptStart).Milliseconds(),
			})
		}
	}

	// last.written is true when a response was committed (success or a stream that
	// began). Otherwise either every target was exhausted (last.retry) or a
	// terminal pre-commit error occurred; surface its envelope.
	if !last.written {
		// Prefer a real compatible-target failure over a trailing materialize skip.
		outcome := last
		if last.logErr == skipLogAttachment && sawRealAttempt && lastReal.status != 0 {
			outcome = lastReal
		} else if last.logErr == skipLogAttachment && !sawRealAttempt {
			outcome = terminal(http.StatusBadRequest, last.errMsg, "invalid_request_error")
		}
		status := outcome.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		// request_failed in the defer covers the final client-facing outcome;
		// upstream_attempt_failed covers each failed provider attempt.
		res.fail(w, ingress, status, outcome.errMsg, outcome.errType)
		res.logErr = outcome.logErr
		// Attachment-only skips never produce usage. A selected real failure
		// that recorded attempt-local tokens owns the final counts so an
		// earlier target's pre-commit decode cannot remain on reqResult.
		if outcome.hasTok {
			res.inTok = outcome.inTok
			res.outTok = outcome.outTok
		}
	}
}

// logUpstreamAttemptFailed emits one structured DEBUG event per failed target.
func (p *Proxy) logUpstreamAttemptFailed(ctx context.Context, combo string, provider *domain.Provider, upstreamModel string, backend codec, attempt int, last attemptResult, retry bool) {
	observability.Logger(ctx, p.logger).Debug("upstream_attempt_failed",
		"event", "upstream_attempt_failed",
		"combo", combo,
		"provider", provider.Name,
		"upstream_model", upstreamModel,
		"protocol", string(provider.Protocol),
		"format", backend.id,
		"attempt", attempt,
		"status", last.status,
		"error", last.logErr,
		"retry", retry,
	)
}

// orderTargets returns the combo's targets in the order the resolution loop
// should try them, plus the first structural attachment incompatibility reason
// observed when every enabled target was filtered out (empty otherwise).
// Failover keeps position order; round-robin rotates the start by a per-combo
// counter, then continues through the remainder so it still fails over past a
// dead target. In both cases, disabled targets and archived providers are
// dropped entirely (unlike backoff, which only defers). Providers whose
// transport cannot represent the request's attachments are dropped before any
// backoff skip credit is consumed, and each such drop is logged. Providers
// penalized for recent pre-commit failures are deferred behind healthy ones for
// a number of subsequent requests (stably, preserving relative order) so a
// persistently failing target is not retried first every request. Penalized
// targets are only deferred, never dropped, so an all-backed-off combo still
// resolves and retries its least-bad option among attachment-compatible targets.
func (p *Proxy) orderTargets(ctx context.Context, combo *domain.Combo, ingress codec, prep *attachmentPrep) ([]domain.ComboTarget, string) {
	// Disabled targets and archived providers are explicit user choices: drop
	// them from resolution entirely (unlike backoff, which only defers). The
	// Provider nil-guard keeps unit tests that omit the hydrated provider working.
	enabled := make([]domain.ComboTarget, 0, len(combo.Targets))
	for _, t := range combo.Targets {
		if t.Enabled && (t.Provider == nil || !t.Provider.Archived) {
			enabled = append(enabled, t)
		}
	}

	// Structural attachment filter runs before health ordering so incompatible
	// providers never consume skip credits.
	compatible := enabled
	var firstFilterReason string
	if prep != nil && prep.hasAttachments() {
		compatible = make([]domain.ComboTarget, 0, len(enabled))
		for _, t := range enabled {
			backend := openaiCodec
			if t.Provider != nil {
				backend = backendCodec(t.Provider.Protocol)
			}
			translated := ingress.id != backend.id
			if reason := prep.checkCompatible(backend, translated); reason != "" {
				if firstFilterReason == "" {
					firstFilterReason = reason
				}
				name, proto := "", ""
				if t.Provider != nil {
					name = t.Provider.Name
					proto = string(t.Provider.Protocol)
				}
				observability.Logger(ctx, p.logger).Debug("attachment_target_skipped",
					"event", "attachment_target_skipped",
					"combo", combo.Name,
					"provider", name,
					"upstream_model", t.UpstreamModel,
					"protocol", proto,
					"format", backend.id,
					"reason", reason,
				)
				continue
			}
			compatible = append(compatible, t)
		}
	}

	if len(compatible) == 0 {
		return nil, firstFilterReason
	}
	if len(compatible) == 1 {
		return compatible, ""
	}
	base := compatible
	if combo.Strategy == domain.StrategyRoundRobin {
		start := p.nextRoundRobin(combo.ID, len(compatible))
		base = make([]domain.ComboTarget, 0, len(compatible))
		for i := range compatible {
			base = append(base, compatible[(start+i)%len(compatible)])
		}
	}
	// Consume at most one skip credit per unique provider per request: a provider
	// appearing in multiple targets of one combo must not be charged twice.
	// Only attachment-compatible candidates participate.
	seen := make(map[int64]bool, len(base))
	healthy := make([]domain.ComboTarget, 0, len(base))
	backedOff := make([]domain.ComboTarget, 0, len(base))
	for _, t := range base {
		off, ok := seen[t.ProviderID]
		if !ok {
			off = p.providerBackedOff(t.ProviderID)
			seen[t.ProviderID] = off
		}
		if off {
			backedOff = append(backedOff, t)
		} else {
			healthy = append(healthy, t)
		}
	}
	return append(healthy, backedOff...), ""
}

// servePassthrough forwards the body unchanged except for the model rewrite,
// preserving any provider-specific fields the IR does not model. The upstream
// response is relayed as-is since its format already matches the ingress.
func (p *Proxy) servePassthrough(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, upstreamModel string, body []byte, clientHeaders http.Header, prep *attachmentPrep) attemptResult {
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

	status, respBody, err := p.forward(ctx, provider, ingress.upstreamPath, rewritten, clientHeaders)
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	if status < 200 || status >= 300 {
		return retryableUpstreamStatus(status, respBody)
	}
	res.status = status
	// Usage is not modeled in passthrough; recover it best-effort from the raw
	// body, which already matches the ingress format.
	res.inTok, res.outTok = parseUsage(respBody, ingress.id)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
	return committed()
}

// serveTranslated converts the request to the backend protocol, forwards it, and
// converts the response back to the ingress protocol.
func (p *Proxy) serveTranslated(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, body []byte, prep *attachmentPrep) attemptResult {
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
	if captured := thinking.Capture(body); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	req.Stream = false

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to encode upstream request", "api_error")
	}
	upstreamBody, err = finalizeEncodedBody(upstreamBody, req, backend, provider)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to finalize upstream request", "api_error")
	}
	upstreamBody, err = prepareUpstreamRequest(ctx, backend, provider, upstreamBody)
	if err != nil {
		return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	if backend.streamOnly {
		return p.serveStreamOnlyUnary(w, ctx, res, ingress, backend, provider, upstreamModel, upstreamBody)
	}

	status, respBody, err := p.forward(ctx, provider, backend.upstreamPath, upstreamBody, nil)
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	if status < 200 || status >= 300 {
		return retryableUpstreamStatus(status, respBody)
	}

	resp, err := backend.decodeResponse(respBody)
	if err != nil {
		if _, ok := ir.AsStreamFailure(err); ok {
			return retryableStreamFailure(err)
		}
		return retryable(http.StatusBadGateway, "failed to decode upstream response", "api_error")
	}
	res.inTok = resp.Usage.InputTokens
	res.outTok = resp.Usage.OutputTokens
	out, err := ingress.encodeResponse(resp)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to encode response", "api_error")
	}
	res.status = http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return committed()
}

// serveStreamOnlyUnary handles a stream-only backend (Codex, Kiro) for a
// non-streaming client request: it sends the request with the backend's stream
// Accept, decodes the upstream stream into an IR response, then renders the
// ingress format's unary response envelope.
func (p *Proxy) serveStreamOnlyUnary(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, upstreamBody []byte) attemptResult {
	var writeFrame func([]byte) error
	closeWrite := func() {}
	var resp *http.Response
	var err error
	if backend.decodeStreamDuplex != nil {
		resp, writeFrame, closeWrite, err = p.forwardStreamDuplex(ctx, provider, backend.upstreamPath, upstreamBody, nil, backend.streamAccept)
	} else {
		resp, err = p.forwardStream(ctx, provider, backend.upstreamPath, upstreamBody, nil, backend.streamAccept)
	}
	defer closeWrite()
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorMax))
		return retryableUpstreamStatus(resp.StatusCode, errBody)
	}
	irResp, err := collectStreamResponse(resp.Body, backend, writeFrame, upstreamModel)
	if err != nil {
		// Client disconnect: duplex streams surface it as a body-closed read
		// error (see forwardStreamDuplex). Stop quietly instead of failing
		// over against a dead context.
		if ctx.Err() != nil {
			return committed()
		}
		if _, ok := ir.AsStreamFailure(err); ok {
			return retryableStreamFailure(err)
		}
		return retryableStreamDecode(err)
	}
	out, err := ingress.encodeResponse(irResp)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to encode response", "api_error")
	}
	res.status = http.StatusOK
	res.inTok = irResp.Usage.InputTokens
	res.outTok = irResp.Usage.OutputTokens
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return committed()
}

// collectStreamResponse folds backend stream events into a unary IR response.
// It is used only for backends that are SSE-only even for non-streaming clients.
// writeFrame is non-nil only for duplex backends (Cursor AgentService), whose
// streams need mid-stream client replies to finish.
func collectStreamResponse(r io.Reader, backend codec, writeFrame func([]byte) error, fallbackModel string) (*ir.Response, error) {
	resp := &ir.Response{ID: ir.NewID("resp_"), Model: fallbackModel, StopReason: ir.StopEndTurn}
	var text strings.Builder
	type toolBuf struct {
		id   string
		name string
		args strings.Builder
	}
	tools := map[int]*toolBuf{}
	var order []int
	sawEvent := false
	decode := backend.decodeStream
	if backend.decodeStreamDuplex != nil {
		decode = func(r io.Reader, emit func(ir.StreamEvent) error) error {
			return backend.decodeStreamDuplex(r, writeFrame, emit)
		}
	}
	err := decode(r, func(ev ir.StreamEvent) error {
		sawEvent = true
		switch ev.Kind {
		case ir.EventMessageStart:
			if ev.ID != "" {
				resp.ID = ev.ID
			}
			if ev.Model != "" {
				resp.Model = ev.Model
			}
			if ev.InputTokens != 0 {
				resp.Usage.InputTokens = ev.InputTokens
			}
		case ir.EventTextDelta:
			text.WriteString(ev.Text)
		case ir.EventToolCallStart:
			if _, ok := tools[ev.Index]; !ok {
				order = append(order, ev.Index)
			}
			tools[ev.Index] = &toolBuf{id: ev.ToolID, name: ev.ToolName}
		case ir.EventToolCallDelta:
			tb := tools[ev.Index]
			if tb == nil {
				tb = &toolBuf{}
				tools[ev.Index] = tb
				order = append(order, ev.Index)
			}
			tb.args.WriteString(ev.ArgsFrag)
		case ir.EventFinish:
			if ev.InputTokens != 0 {
				resp.Usage.InputTokens = ev.InputTokens
			}
			resp.Usage.OutputTokens = ev.OutputTokens
			if ev.StopReason != "" {
				resp.StopReason = ev.StopReason
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A stream that decoded cleanly but produced nothing is an upstream failure,
	// not an empty assistant turn: without this the caller renders a successful
	// empty response instead of failing over.
	if !sawEvent {
		return nil, &ir.StreamFailure{Message: "upstream returned an empty stream"}
	}
	if text.Len() > 0 {
		resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockText, Text: text.String()})
	}
	for _, idx := range order {
		tb := tools[idx]
		args := []byte(tb.args.String())
		if len(args) == 0 || !json.Valid(args) {
			args = []byte("{}")
		}
		resp.Content = append(resp.Content, ir.ContentBlock{
			Type: ir.BlockToolUse, ToolID: tb.id, ToolName: tb.name, ToolInput: json.RawMessage(args),
		})
	}
	return resp, nil
}

// parseUsage recovers token counts from a unary response body's top-level usage
// object. codecID selects one alias family so hybrid OpenAI/Anthropic field sets
// are not summed twice. Returns 0,0 when no usage object is present.
func parseUsage(body []byte, codecID string) (in, out int) {
	var u struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &u) != nil || len(u.Usage) == 0 {
		return 0, 0
	}
	return parseUsageObject(u.Usage, codecID)
}

// parseUsageObject interprets one usage JSON object under a single alias family.
//
// Families:
//   - oai-chat: prompt_tokens / completion_tokens
//   - oai-responses / oai-codex: input_tokens / output_tokens (no Anthropic caches)
//   - anth-msg / claude-code: input_tokens + cache_* / output_tokens
//
// Without useful codec context, prefer a complete prompt/completion pair when
// both sides are present; otherwise use input/output. Anthropic cache fields are
// folded into input only for Anthropic codecs, or in the fallback when the
// input/output family is selected and no prompt/completion pair won.
func parseUsageObject(raw json.RawMessage, codecID string) (in, out int) {
	if len(raw) == 0 || raw[0] != '{' {
		return 0, 0
	}
	var f struct {
		PromptTokens             int `json:"prompt_tokens"`
		CompletionTokens         int `json:"completion_tokens"`
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return 0, 0
	}
	switch codecID {
	case "oai-chat":
		return f.PromptTokens, f.CompletionTokens
	case "oai-responses", "oai-codex":
		return f.InputTokens, f.OutputTokens
	case "anth-msg", "claude-code":
		return f.InputTokens + f.CacheCreationInputTokens + f.CacheReadInputTokens, f.OutputTokens
	}
	// Fallback without codec context: pick one family, never sum aliases.
	hasPromptFamily := f.PromptTokens != 0 || f.CompletionTokens != 0
	hasIOFamily := f.InputTokens != 0 || f.OutputTokens != 0 || f.CacheCreationInputTokens != 0 || f.CacheReadInputTokens != 0
	if hasPromptFamily && (!hasIOFamily || (f.PromptTokens != 0 && f.CompletionTokens != 0)) {
		return f.PromptTokens, f.CompletionTokens
	}
	if hasIOFamily {
		// Treat as Anthropic-shaped when cache fields appear; otherwise plain I/O
		// (Responses). Cache fields alone still count as input.
		return f.InputTokens + f.CacheCreationInputTokens + f.CacheReadInputTokens, f.OutputTokens
	}
	return 0, 0
}

// upstreamErrorMessage extracts a human message from an upstream error body.
// Both OpenAI and Anthropic nest it under error.message.
func upstreamErrorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	if len(body) > 0 {
		return string(body)
	}
	return "upstream error"
}

// recordLog persists a request log fire-and-forget so a slow DB write never
// blocks the response. It runs on a fresh context since the request's may be
// done by the time the write lands.
func (p *Proxy) recordLog(ctx context.Context, l *domain.RequestLog) {
	logger := observability.Logger(ctx, p.logger)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.store.CreateRequestLog(ctx, l); err != nil {
			logger.Error("request_log_write_failed",
				"event", "request_log_write_failed",
				"error", err,
			)
		}
	}()
}

func writeErr(w http.ResponseWriter, c codec, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(c.encodeError(message, errType))
}

// authenticate verifies the bearer/x-api-key token against stored access keys,
// returning the key's label on success. When no access keys exist the proxy
// runs in open mode and accepts every request unauthenticated.
func (p *Proxy) authenticate(r *http.Request) (string, bool) {
	token := extractToken(r)
	if token != "" {
		if key, err := p.store.VerifyToken(r.Context(), token); err == nil {
			return key.Name, true
		}
	}
	// No valid token: allow only when there are no keys configured at all.
	if n, err := p.store.CountAccessKeys(r.Context()); err == nil && n == 0 {
		return "(open)", true
	}
	return "", false
}

func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
