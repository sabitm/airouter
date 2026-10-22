package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/media"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/thinking"
	"airouter/internal/store"
)

const maxBodyBytes = 64 << 20 // 64 MiB ceiling on inbound request bodies

// usageEstimateCharsPerToken is the 9router character estimate. It is not a
// tokenizer and must not replace an upstream usage object.
const usageEstimateCharsPerToken = 4

const (
	maxCollectedStreamResponseBytes = maxUnaryUpstreamResponseBytes
	maxCollectedStreamToolCalls     = 1024
	// maxCollectedStreamBlocks bounds indexed thinking plus redacted blocks so
	// many distinct indices or a flood of empty redacted events cannot grow
	// the extras bookkeeping without bound.
	maxCollectedStreamBlocks = 1024
	// collectedBlockOverhead approximates the per-block map entry, order slot,
	// and builder cost billed to the byte budget before a block is allocated.
	// Updates to an existing block never bill it again.
	collectedBlockOverhead = 128
	// maxCollectedStreamReasoning bounds structured Responses reasoning items.
	// Summary parts share the same flat overhead and are counted per part.
	maxCollectedStreamReasoning      = 1024
	maxCollectedStreamReasoningParts = 1024
	collectedReasoningPartOverhead   = 128
	collectedReasoningKeyOverhead    = 32
	// collectedCitationOverhead bills the Citation struct and slice slot in
	// addition to the raw object bytes, so many tiny citations stay bounded.
	collectedCitationOverhead = 64
)

var (
	errCollectedStreamResponseTooLarge = errors.New("upstream stream response too large")
	errCollectedStreamTooManyTools     = errors.New("upstream stream response has too many tool calls")
)

// upstreamErrorMax caps how many bytes of an upstream error body are read for
// extracting a client-facing message and persisted request-history detail. HAR
// capture is independently bounded by harlog.MaxBody.
const upstreamErrorMax = domain.MaxErrorMessageBytes

// reqResult accumulates the outcome of one request for logging. Each serve path
// fills it in; serve records a RequestLog once on completion.
type reqResult struct {
	status int
	inTok  int
	outTok int
	// usageEstimated is true when inTok/outTok were derived from character
	// length because the upstream sent no usage. It must not replace a real
	// upstream count, and it is not written into the client response.
	usageEstimated bool
	errMsg         string
	logErr         string
	// anthOrdinary/anthCache* are request-local Anthropic stream partitions for
	// passthrough sniffing. Written only from committed/relayed events.
	anthOrdinary   int
	anthCacheRead  int
	anthCacheWrite int
	// opaqueOmitted is set after one metadata diagnostic for projected-away
	// Anthropic signature or redacted thinking. Failover attempts share it.
	opaqueOmitted bool
}

// clampErrorMessage keeps client and request-history messages within the error
// body budget without cutting a valid UTF-8 sequence at the byte boundary.
func clampErrorMessage(message string) string {
	if !utf8.ValidString(message) {
		message = strings.ToValidUTF8(message, "\uFFFD")
	}
	if len(message) <= upstreamErrorMax {
		return message
	}
	end := upstreamErrorMax
	for end > 0 && !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end]
}

// fail writes an error envelope and records the failure on the result.
func (res *reqResult) fail(w http.ResponseWriter, ingress codec, status int, message, errType string) {
	message = clampErrorMessage(message)
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
	// usageEstimated is true when inTok/outTok came from character length
	// because the upstream sent no usage. Real upstream counts leave it false.
	usageEstimated bool
}

func (ar attemptResult) withTokens(in, out int) attemptResult {
	ar.inTok = in
	ar.outTok = out
	ar.hasTok = true
	return ar
}

func (ar attemptResult) withEstimatedTokens(in, out int) attemptResult {
	ar = ar.withTokens(in, out)
	ar.usageEstimated = true
	return ar
}

func committed() attemptResult { return attemptResult{written: true} }

// committedFailure stops failover after bytes were written, keeps the provider
// penalized, and surfaces errMsg in request history. status is the HTTP status
// already committed (typically 200 for SSE).
func committedFailure(status int, errMsg, logErr string) attemptResult {
	errMsg = clampErrorMessage(errMsg)
	if logErr == "" {
		logErr = errMsg
	} else {
		logErr = clampErrorMessage(logErr)
	}
	return attemptResult{written: true, failed: true, status: status, errMsg: errMsg, logErr: logErr, errType: "api_error"}
}

func retryable(status int, message, errType string) attemptResult {
	message = clampErrorMessage(message)
	return attemptResult{retry: true, status: status, errMsg: message, logErr: message, errType: errType}
}

// retryableUpstreamStatus preserves a structured upstream message for the
// client and request history while keeping raw bodies in bounded HAR only.
func retryableUpstreamStatus(status int, body []byte) attemptResult {
	if len(body) > upstreamErrorMax {
		body = body[:upstreamErrorMax]
	}
	return attemptResult{
		retry:   true,
		status:  status,
		errMsg:  clampErrorMessage(upstreamErrorMessage(status, body)),
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
		errMsg:  clampErrorMessage("upstream stream decode failed: " + err.Error()),
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
		errMsg:  clampErrorMessage(errMsg),
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
	message = clampErrorMessage(message)
	return attemptResult{status: status, errMsg: message, logErr: message, errType: errType}
}

// terminalEncodeError maps a backend.encodeRequest failure. A typed request
// validation error is invalid caller data discovered locally: HTTP 400 with no
// upstream contact and no failover. Any other failure is an internal encoding
// fault and stays a generic HTTP 500 with the raw detail withheld.
func terminalEncodeError(err error) attemptResult {
	if _, ok := ir.AsRequestValidation(err); ok {
		return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	return terminal(http.StatusInternalServerError, "failed to encode upstream request", "api_error")
}

// retryableResponseIncompatibility maps an upstream response that the ingress
// format cannot represent to a retryable backend fault. The fixed message keeps
// server-block content and provider metadata out of client and terminal output.
func retryableResponseIncompatibility(in, out int, estimated bool) attemptResult {
	ar := retryable(http.StatusBadGateway, "upstream response is not representable in the ingress format", "api_error")
	if estimated {
		return ar.withEstimatedTokens(in, out)
	}
	return ar.withTokens(in, out)
}

// terminalResponseEncodeError keeps usage decoded before an internal encoder
// fault on the attempt row. It stays terminal and non-retryable, and an
// explicit zero is reported as known usage rather than a missing estimate.
func terminalResponseEncodeError(in, out int, estimated bool) attemptResult {
	ar := terminal(http.StatusInternalServerError, "failed to encode response", "api_error")
	if estimated {
		return ar.withEstimatedTokens(in, out)
	}
	return ar.withTokens(in, out)
}

func callerCanceledResult() attemptResult {
	return attemptResult{status: http.StatusBadGateway, errMsg: "request canceled", logErr: "request canceled", errType: "api_error"}
}

func ctxErr(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

// serve runs the full ingress lifecycle for one request. ingress is the codec
// for the endpoint the client called.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, ingress codec) {
	if traceInfoFrom(r.Context()) == nil {
		r = r.WithContext(WithTraceInfo(r.Context(), &TraceInfo{}))
	}
	start := time.Now()
	res := &reqResult{status: http.StatusOK}
	rec := &domain.RequestLog{Format: ingress.id}
	defer func() {
		rec.Status = res.status
		rec.InputTokens = res.inTok
		rec.OutputTokens = res.outTok
		rec.UsageEstimated = res.usageEstimated
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

	auth, ok := p.authenticate(r)
	if !ok {
		res.fail(w, ingress, http.StatusUnauthorized, "invalid or missing access key", "authentication_error")
		return
	}
	rec.AccessKeyName = auth.Name

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
	// A unique active target is unambiguous route metadata even when local
	// validation rejects the request before target ordering or upstream contact.
	if targets := activeComboTargets(combo); len(targets) == 1 && targets[0].Provider != nil {
		rec.Provider = targets[0].Provider.Name
		rec.UpstreamModel = targets[0].UpstreamModel
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
		if msg, ok := emptyCandidateMessage(combo, &prep, filterReason, meta.Model); ok {
			res.fail(w, ingress, http.StatusBadRequest, msg, "invalid_request_error")
			return
		}
		res.fail(w, ingress, http.StatusInternalServerError, "combo has no targets: "+meta.Model, "api_error")
		return
	}
	if hasOpencodeTarget(candidates) {
		// Capture only for requests that can reach OpenCode. The original body is
		// needed because translation drops unknown session metadata.
		r = r.WithContext(withOpencodeRequest(r.Context(), p.opencodeNonce, r.Header, body))
	}
	if hasCodexTarget(candidates) {
		// Capture Codex identity from the original headers/body before translation
		// drops unknown session fields. Tenant scope is the verified access-key
		// hash, or openTenantScope in open mode.
		r = r.WithContext(withCodexRequest(r.Context(), auth.Scope, r.Header, body))
	}
	if hasClaudeCodeTarget(candidates) {
		// Capture Claude Code session identity before translation drops metadata.
		// The account UUID is never taken from this body.
		r = r.WithContext(withClaudeRequest(r.Context(), r.Header, body))
		r = r.WithContext(withClaudeTenant(r.Context(), auth.Scope))
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
		// A canceled caller must not dispatch another provider after the previous
		// attempt completed. The first attempt still handles its own cancellation.
		if i > 0 && ctxErr(r.Context()) {
			last = callerCanceledResult()
			break
		}
		provider := t.Provider
		rec.Provider = provider.Name
		rec.UpstreamModel = t.UpstreamModel
		backend := backendCodecForProvider(provider.Protocol, provider, t.UpstreamModel)

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

		attachmentSkip := last.retry && (last.logErr == skipLogAttachment || last.logErr == skipLogThinking)
		if last.logErr == "request canceled" && ctxErr(r.Context()) {
			break
		}
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
			// The winning attempt owns the final log. Copy its token flags even
			// when a count is zero so an earlier shared estimate cannot remain.
			if last.written && last.hasTok {
				res.inTok = last.inTok
				res.outTok = last.outTok
				res.usageEstimated = last.usageEstimated
			}
			break
		}
		if attachmentSkip {
			// Materialization / late structural skip: no health penalty.
			event := "attachment_target_skipped"
			if last.logErr == skipLogThinking {
				event = "thinking_target_skipped"
			}
			observability.Logger(r.Context(), p.logger).Debug(event,
				"event", event,
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
				AccessKeyName:  rec.AccessKeyName,
				Combo:          rec.Combo,
				Provider:       provider.Name,
				UpstreamModel:  t.UpstreamModel,
				Format:         ingress.id,
				Stream:         meta.Stream,
				Status:         last.status,
				InputTokens:    last.inTok,
				OutputTokens:   last.outTok,
				UsageEstimated: last.usageEstimated,
				ErrMsg:         last.errMsg,
				LatencyMS:      time.Since(attemptStart).Milliseconds(),
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
		} else if (last.logErr == skipLogAttachment || last.logErr == skipLogThinking) && !sawRealAttempt {
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
		// The selected final outcome owns the final counts even when it has no
		// tokens. This prevents an earlier target's usage or estimate from
		// remaining on reqResult.
		res.inTok = outcome.inTok
		res.outTok = outcome.outTok
		res.usageEstimated = outcome.usageEstimated
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

func activeComboTargets(combo *domain.Combo) []domain.ComboTarget {
	if combo == nil {
		return nil
	}
	targets := make([]domain.ComboTarget, 0, len(combo.Targets))
	for _, t := range combo.Targets {
		if t.Enabled && (t.Provider == nil || !t.Provider.Archived) {
			targets = append(targets, t)
		}
	}
	return targets
}

func hasOpencodeTarget(targets []domain.ComboTarget) bool {
	for _, target := range targets {
		if target.Provider != nil && target.Provider.Protocol == domain.ProtocolOpencode {
			return true
		}
	}
	return false
}

// ambiguousResponsesRoute returns a message when compatible targets span more
// than one provider or upstream model. Duplicate identical destinations are
// allowed; every distinct provider ID or model makes replay unsafe.
func ambiguousResponsesRoute(targets []domain.ComboTarget) string {
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		key := strconv.FormatInt(t.ProviderID, 10) + "\x00" + t.UpstreamModel
		seen[key] = true
		if len(seen) > 1 {
			return responsesStateAmbiguous
		}
	}
	return ""
}

// emptyCandidateMessage distinguishes a combo with no enabled targets from
// a combo whose enabled targets were all filtered. The second form is a 400.
// Attachment and thinking reasons stay distinct when both filters apply.
func emptyCandidateMessage(combo *domain.Combo, prep *attachmentPrep, filterReason, _ string) (string, bool) {
	if len(activeComboTargets(combo)) == 0 {
		return "", false
	}
	if prep == nil {
		return "", false
	}
	if prep.needsThinkingPreservation() || prep.needsResponsesPreservation() || prep.needsAnthropicState() {
		if filterReason != "" {
			return filterReason, true
		}
		if prep.needsThinkingPreservation() {
			return thinkingHistoryNoProvider, true
		}
		if prep.needsResponsesPreservation() {
			return responsesStateNoProvider, true
		}
		return anthropicStateUnsupported, true
	}
	// Attachment-only incompatibility keeps its dedicated message envelope.
	if prep.hasAttachments() {
		msg := "attachment not supported by upstream: no compatible provider in combo"
		if filterReason != "" {
			msg = "attachment not supported by upstream: " + filterReason
		}
		return msg, true
	}
	return "", false
}

// orderTargets returns the combo's targets in the order the resolution loop
// should try them, plus the first structural incompatibility reason observed
// when every enabled target was filtered out (empty otherwise).
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
	enabled := activeComboTargets(combo)

	// Structural attachment and thinking filters run before health ordering so
	// incompatible providers never consume skip credits or receive penalties.
	compatible := enabled
	var firstFilterReason string
	needAttach := prep != nil && prep.hasAttachments()
	needThink := prep != nil && prep.needsThinkingPreservation()
	needResponses := prep != nil && prep.needsResponsesPreservation()
	needAnthropic := prep != nil && prep.needsAnthropicState()
	if needAttach || needThink || needResponses || needAnthropic {
		compatible = make([]domain.ComboTarget, 0, len(enabled))
		for _, t := range enabled {
			backend := openaiCodec
			if t.Provider != nil {
				backend = backendCodecForProvider(t.Provider.Protocol, t.Provider, t.UpstreamModel)
			}
			translated := ingress.id != backend.id
			reason := ""
			event := "attachment_target_skipped"
			if needAttach {
				reason = prep.checkCompatible(backend, translated)
			}
			if reason == "" && needThink {
				reason = prep.checkThinking(backend)
				if reason != "" {
					event = "thinking_target_skipped"
				}
			}
			if reason == "" && needResponses {
				reason = prep.checkThinking(backend)
				if reason != "" {
					event = "thinking_target_skipped"
				}
			}
			if reason == "" && needAnthropic {
				reason = prep.checkThinking(backend)
				if reason != "" {
					event = "thinking_target_skipped"
				}
			}
			if reason != "" {
				if firstFilterReason == "" {
					firstFilterReason = reason
				}
				name, proto := "", ""
				if t.Provider != nil {
					name = t.Provider.Name
					proto = string(t.Provider.Protocol)
				}
				observability.Logger(ctx, p.logger).Debug(event,
					"event", event,
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
	// Provider-owned Responses state must reach exactly one provider/model.
	// The check runs before health ordering so an ambiguous request consumes no
	// backoff skip credit and contacts no upstream.
	if needResponses {
		if msg := ambiguousResponsesRoute(compatible); msg != "" {
			return nil, msg
		}
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
	if reason := prep.checkThinking(ingress); reason != "" {
		return thinkingSkip(reason)
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
	inTok, outTok := parseUsage(respBody, ingress.id)
	res.inTok, res.outTok = inTok, outTok
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
	return committed().withTokens(inTok, outTok)
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
	if reason := prep.checkThinking(backend); reason != "" {
		return thinkingSkip(reason)
	}
	if err := prep.materialize(ctx, req, backend); err != nil {
		return materializeSkip(err)
	}
	if captured := thinking.Capture(body); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	req.Model = applyClaudeCodeTargetModel(ctx, provider, req.Model)
	// Encode the upstream transport: stream-only backends collect SSE and
	// require stream:true on the wire; regular unary backends stay false.
	req.Stream = backend.streamOnly

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		return terminalEncodeError(err)
	}
	upstreamBody, err = finalizeEncodedBody(upstreamBody, req, backend, provider)
	if err != nil {
		if opencodeBudgetError(err) {
			return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
		}
		return terminal(http.StatusInternalServerError, "failed to finalize upstream request", "api_error")
	}
	upstreamBody, err = p.prepareUpstreamRequestFor(ctx, backend, provider, req, upstreamBody)
	if err != nil {
		if kiroPrepareCanceled(ctx, backend, err) {
			return callerCanceledResult()
		}
		return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	if backend.streamOnly {
		return p.serveStreamOnlyUnary(w, ctx, res, ingress, backend, provider, upstreamModel, upstreamBody, req)
	}

	status, respBody, err := p.forward(ctx, provider, backend.upstreamPath, upstreamBody, nil)
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	if status < 200 || status >= 300 {
		return retryableUpstreamStatus(status, respBody)
	}

	decodeResponse := backend.decodeResponse
	if isOpencodeCodec(backend.id) {
		decodeResponse = opencode.DecodeResponse(decodeResponse, opencodeToolMapFrom(ctx))
	}
	resp, err := decodeResponse(respBody)
	if err != nil {
		if _, ok := ir.AsStreamFailure(err); ok {
			return retryableStreamFailure(err)
		}
		return retryable(http.StatusBadGateway, "failed to decode upstream response", "api_error")
	}
	inTok, outTok := resp.Usage.InputTokens, resp.Usage.OutputTokens
	if responseDropsOpaque(resp.Content) {
		noteAnthropicOmission(res, observability.Logger(ctx, p.logger), ingress.id)
	}
	if responseDropsResponsesOpaque(resp.Content) {
		noteResponsesOmission(res, observability.Logger(ctx, p.logger), ingress.id)
	}
	out, err := ingress.encodeResponse(resp)
	if err != nil {
		if _, ok := ir.AsResponseIncompatibility(err); ok {
			return retryableResponseIncompatibility(inTok, outTok, false)
		}
		return terminalResponseEncodeError(inTok, outTok, false)
	}
	res.inTok, res.outTok = inTok, outTok
	res.status = http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return committed().withTokens(inTok, outTok)
}

// serveStreamOnlyUnary handles a stream-only backend (Codex, Kiro) for a
// non-streaming client request: it sends the request with the backend's stream
// Accept, decodes the upstream stream into an IR response, then renders the
// ingress format's unary response envelope.
func (p *Proxy) serveStreamOnlyUnary(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, upstreamBody []byte, req *ir.Request) attemptResult {
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
	irResp, err := collectStreamResponse(resp.Body, opencodeBackendForAttempt(ctx, backend), writeFrame, upstreamModel, req.Tools)
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
	if backend.protocol == domain.ProtocolCursor {
		applyCursorUsage(req, irResp, len(upstreamBody))
	}
	if responseDropsOpaque(irResp.Content) {
		noteAnthropicOmission(res, observability.Logger(ctx, p.logger), ingress.id)
	}
	if responseDropsResponsesOpaque(irResp.Content) {
		noteResponsesOmission(res, observability.Logger(ctx, p.logger), ingress.id)
	}
	// Collect-only usage is decoded before the ingress encoder can reject the
	// response, so a failed attempt reports its own counts. Do not add the
	// generic 0/0 estimate to an unrepresentable response.
	inTok, outTok := irResp.Usage.InputTokens, irResp.Usage.OutputTokens
	usageEstimated := false
	if backend.protocol == domain.ProtocolCursor {
		// applyCursorUsage already chose raw or estimate. Do not run the
		// request-byte heuristic: Cursor adds context the proxy did not send.
		usageEstimated = !irResp.Usage.UsageReported
	} else if !usageInputPlausible(len(upstreamBody), inTok) {
		inTok = 0
	}
	out, err := ingress.encodeResponse(irResp)
	if err != nil {
		if _, ok := ir.AsResponseIncompatibility(err); ok {
			return retryableResponseIncompatibility(inTok, outTok, usageEstimated)
		}
		return terminalResponseEncodeError(inTok, outTok, usageEstimated)
	}
	if int64(len(out)) > maxUnaryUpstreamResponseBytes {
		return retryable(http.StatusBadGateway, "upstream response too large", "api_error")
	}
	// A reported Cursor 0/0 is explicit. Do not replace it with the generic estimate.
	if inTok == 0 && outTok == 0 && !usageEstimated && (backend.protocol != domain.ProtocolCursor || !irResp.Usage.UsageReported) {
		inTok, outTok = estimateUsage(req, irResp)
		usageEstimated = true
	}
	res.status = http.StatusOK
	res.inTok = inTok
	res.outTok = outTok
	res.usageEstimated = usageEstimated
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	if usageEstimated {
		return committed().withEstimatedTokens(inTok, outTok)
	}
	return committed().withTokens(inTok, outTok)
}

// collectStreamResponse folds backend stream events into a unary IR response.
// It is used only for backends that are SSE-only even for non-streaming clients.
// writeFrame is non-nil only for duplex backends (Cursor AgentService), whose
// streams need mid-stream client replies to finish.
func applyCollectedUsage(u *ir.Usage, ev ir.StreamEvent) {
	if ev.InputTokens != 0 {
		u.InputTokens = ev.InputTokens
	}
	if ev.CacheReadTokens != 0 {
		u.CacheReadTokens = ev.CacheReadTokens
	}
	if ev.CacheWriteTokens != 0 {
		u.CacheWriteTokens = ev.CacheWriteTokens
	}
	// Cursor sets this only on a normal finish with a complete counter pair.
	// A later event without the marker must not clear a reported finish, and
	// a false default must not erase another codec's collected counts.
	if ev.UsageReported {
		u.UsageReported = true
	}
}

// applyCollectedResponseState merges provider-owned response continuity fields
// from message_start and message_delta. A later non-empty value replaces an
// earlier one; absent values keep the earlier state.
func applyCollectedResponseState(resp *ir.Response, ev ir.StreamEvent, reserve func(int) error) error {
	if len(ev.Container) == 0 && len(ev.ContextManagement) == 0 && len(ev.StopDetails) == 0 && len(ev.InputTransformations) == 0 {
		return nil
	}
	replace := func(dst *json.RawMessage, value json.RawMessage) error {
		if len(value) == 0 {
			return nil
		}
		if delta := len(value) - len(*dst); delta > 0 {
			if err := reserve(delta); err != nil {
				return err
			}
		}
		*dst = append(json.RawMessage(nil), value...)
		return nil
	}
	if resp.Anthropic == nil {
		resp.Anthropic = &ir.AnthropicResponseState{}
	}
	if err := replace(&resp.Anthropic.Container, ev.Container); err != nil {
		return err
	}
	if err := replace(&resp.Anthropic.ContextManagement, ev.ContextManagement); err != nil {
		return err
	}
	if err := replace(&resp.Anthropic.StopDetails, ev.StopDetails); err != nil {
		return err
	}
	return replace(&resp.Anthropic.InputTransformations, ev.InputTransformations)
}

func opencodeBackendForAttempt(ctx context.Context, backend codec) codec {
	if !isOpencodeCodec(backend.id) {
		return backend
	}
	mapping := opencodeToolMapFrom(ctx)
	if !mapping.Active() {
		return backend
	}
	backend.decodeStream = opencode.DecodeStream(backend.decodeStream, mapping)
	backend.decodeResponse = opencode.DecodeResponse(backend.decodeResponse, mapping)
	return backend
}

func collectStreamResponse(r io.Reader, backend codec, writeFrame func([]byte) error, fallbackModel string, clientTools []ir.Tool) (*ir.Response, error) {
	return collectStreamResponseWithLimits(r, backend, writeFrame, fallbackModel, clientTools, maxCollectedStreamResponseBytes, maxCollectedStreamToolCalls)
}

func collectStreamResponseWithLimits(r io.Reader, backend codec, writeFrame func([]byte) error, fallbackModel string, clientTools []ir.Tool, maxBytes int64, maxTools int) (*ir.Response, error) {
	resp := &ir.Response{ID: ir.NewID("resp_"), Model: fallbackModel, StopReason: ir.StopEndTurn}
	retainedBytes := int64(len(resp.ID) + len(resp.Model))
	if retainedBytes > maxBytes {
		return nil, errCollectedStreamResponseTooLarge
	}
	reserve := func(delta int) error {
		if delta > 0 && int64(delta) > maxBytes-retainedBytes {
			return errCollectedStreamResponseTooLarge
		}
		retainedBytes += int64(delta)
		return nil
	}
	replace := func(dst *string, value string) error {
		if err := reserve(len(value) - len(*dst)); err != nil {
			return err
		}
		*dst = value
		return nil
	}
	// Content blocks are appended in arrival order so reasoning, text, and tool
	// items keep the wire order. Auxiliary bookkeeping only locates a block; it
	// never regroups the final slice. Text accumulates in a builder so a
	// fragmented stream does not reseat a growing string on every fragment.
	// Every text block charges its overhead before allocation and the block
	// count is bounded independently of the byte budget.
	// Text blocks are keyed by source block index. The key -1 is the legacy
	// unindexed stream, which keeps its old adjacent-merge behavior. Each open
	// state owns one output block so a later source block can never merge its
	// text or citations into an earlier block.
	type collectedText struct {
		blockIdx int
		text     strings.Builder
	}
	texts := map[int]*collectedText{}
	plainBlocks := 0
	newTextState := func() (*collectedText, error) {
		if plainBlocks >= maxCollectedStreamBlocks {
			return nil, errCollectedStreamResponseTooLarge
		}
		if err := reserve(collectedBlockOverhead); err != nil {
			return nil, err
		}
		resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockText})
		plainBlocks++
		return &collectedText{blockIdx: len(resp.Content) - 1}, nil
	}
	ensureTextState := func(index int) (*collectedText, error) {
		if st := texts[index]; st != nil {
			return st, nil
		}
		st, err := newTextState()
		if err != nil {
			return nil, err
		}
		texts[index] = st
		return st, nil
	}
	flushTextState := func(index int) {
		st := texts[index]
		if st == nil {
			return
		}
		resp.Content[st.blockIdx].Text = st.text.String()
		delete(texts, index)
	}
	flushText := func() {
		for index := range texts {
			flushTextState(index)
		}
	}
	appendText := func(index int, text string) error {
		// Legacy empty text events never create a block. An indexed start
		// already created its block, so empty indexed deltas do not add one.
		if text == "" {
			return nil
		}
		if err := reserve(len(text)); err != nil {
			return err
		}
		st, err := ensureTextState(index)
		if err != nil {
			return err
		}
		st.text.WriteString(text)
		return nil
	}
	appendCitation := func(index int, data string) error {
		trimmed := []byte(data)
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return ir.ProtocolError("citation is malformed")
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(trimmed, &head) != nil || head.Type == "" {
			return ir.ProtocolError("citation has no type")
		}
		if err := reserve(len(trimmed) + collectedCitationOverhead); err != nil {
			return err
		}
		st, err := ensureTextState(index)
		if err != nil {
			return err
		}
		resp.Content[st.blockIdx].Citations = append(resp.Content[st.blockIdx].Citations, ir.Citation{
			Type: head.Type, Raw: append(json.RawMessage(nil), trimmed...),
		})
		return nil
	}
	// Indexed thinking and redacted blocks keep their arrival order so an
	// Anthropic-compatible unary rendering can replay signatures exactly.
	// Unsigned empty lifecycle frames never create a block.
	type thinkBlock struct {
		blockIdx  int
		text      strings.Builder
		signature strings.Builder
	}
	thinking := map[int]*thinkBlock{}
	extraBlocks := 0
	extraFor := func(index int) (*thinkBlock, error) {
		if b := thinking[index]; b != nil {
			return b, nil
		}
		if extraBlocks >= maxCollectedStreamBlocks {
			return nil, errCollectedStreamResponseTooLarge
		}
		if err := reserve(collectedBlockOverhead); err != nil {
			return nil, err
		}
		flushText()
		resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockReasoning})
		b := &thinkBlock{blockIdx: len(resp.Content) - 1}
		thinking[index] = b
		extraBlocks++
		return b, nil
	}
	// Structured Responses reasoning binds both the source item ID and the
	// streamed output index so an index-only delta and an ID-bearing done
	// snapshot land on one item. Unkeyed deltas accumulate into one open plain
	// segment; the next unidentified done claims it, and further unidentified
	// done snapshots become distinct items instead of collapsing into one.
	type collectedReasoningPart struct {
		typ  string
		text strings.Builder
	}
	type collectedReasoning struct {
		blockIdx int
		id       string
		hasID    bool
		index    int
		hasIndex bool
		// text retains legacy unindexed readable bytes in arrival order. parts
		// retains structured Responses summary bytes keyed by explicit summary
		// index, so a valid gap stream projects in canonical part order instead
		// of arrival order. legacyParts counts snapshot parts charged for a
		// legacy item that never allocates a parts slice.
		text        strings.Builder
		parts       []*collectedReasoningPart
		structured  bool
		legacyParts int
		seen        *ir.ResponsesReasoning
		complete    bool
	}
	reasoningByID := map[string]*collectedReasoning{}
	reasoningByIndex := map[int]*collectedReasoning{}
	reasoningItems := make([]*collectedReasoning, 0, 8)
	reasoningCount := 0
	// reasoningPartsAlloc counts live per-index parts across every item, so the
	// collector stops at the same global part cap the Responses decoder uses.
	// The retained snapshot clone charges its own part overhead separately.
	reasoningPartsAlloc := 0
	var plainReasoning *collectedReasoning
	newReasoning := func() (*collectedReasoning, error) {
		if reasoningCount >= maxCollectedStreamReasoning {
			return nil, errCollectedStreamResponseTooLarge
		}
		if err := reserve(collectedBlockOverhead); err != nil {
			return nil, err
		}
		flushText()
		resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockReasoning})
		reasoningCount++
		item := &collectedReasoning{blockIdx: len(resp.Content) - 1}
		reasoningItems = append(reasoningItems, item)
		return item, nil
	}
	bindReasoning := func(item *collectedReasoning, ev ir.StreamEvent) error {
		if ev.ReasoningID != "" {
			if item.hasID && item.id != ev.ReasoningID {
				return ir.ProtocolError("conflicting reasoning item identity")
			}
			if other := reasoningByID[ev.ReasoningID]; other != nil && other != item {
				return ir.ProtocolError("conflicting reasoning item identity")
			}
		}
		if ev.ReasoningIndexSet {
			if item.hasIndex && item.index != ev.ReasoningIndex {
				return ir.ProtocolError("conflicting reasoning index")
			}
			if other := reasoningByIndex[ev.ReasoningIndex]; other != nil && other != item {
				return ir.ProtocolError("conflicting reasoning index")
			}
		}
		if ev.ReasoningID != "" && !item.hasID {
			if err := reserve(len(ev.ReasoningID) + collectedReasoningKeyOverhead); err != nil {
				return err
			}
			item.hasID = true
			item.id = ev.ReasoningID
			reasoningByID[ev.ReasoningID] = item
		}
		if ev.ReasoningIndexSet && !item.hasIndex {
			if err := reserve(collectedReasoningKeyOverhead); err != nil {
				return err
			}
			item.hasIndex = true
			item.index = ev.ReasoningIndex
			reasoningByIndex[ev.ReasoningIndex] = item
		}
		return nil
	}
	reasoningFor := func(ev ir.StreamEvent) (*collectedReasoning, error) {
		if ev.ReasoningID != "" {
			if item := reasoningByID[ev.ReasoningID]; item != nil {
				if err := bindReasoning(item, ev); err != nil {
					return nil, err
				}
				return item, nil
			}
		}
		if ev.ReasoningIndexSet {
			if item := reasoningByIndex[ev.ReasoningIndex]; item != nil {
				if err := bindReasoning(item, ev); err != nil {
					return nil, err
				}
				return item, nil
			}
		}
		item, err := newReasoning()
		if err != nil {
			return nil, err
		}
		if err := bindReasoning(item, ev); err != nil {
			return nil, err
		}
		return item, nil
	}
	plainReasoningFor := func() (*collectedReasoning, error) {
		// Reuse the open legacy segment only while it is the tail. Interleaved
		// text must not merge non-adjacent reasoning segments into one block.
		if plainReasoning != nil && !plainReasoning.complete && plainReasoning.blockIdx == len(resp.Content)-1 {
			return plainReasoning, nil
		}
		item, err := newReasoning()
		if err != nil {
			return nil, err
		}
		plainReasoning = item
		return item, nil
	}
	// addReasoningText retains legacy unindexed readable bytes in arrival order.
	addReasoningText := func(item *collectedReasoning, text string) error {
		if text == "" {
			return nil
		}
		if err := reserve(len(text)); err != nil {
			return err
		}
		item.text.WriteString(text)
		return nil
	}
	// structuredPart returns the summary part at index, allocating bounded
	// placeholders for skipped indices. The overhead is charged before the slice
	// grows and each allocated part is charged exactly once.
	structuredPart := func(item *collectedReasoning, index int) (*collectedReasoningPart, error) {
		if index < 0 || index >= maxCollectedStreamReasoningParts {
			return nil, ir.ProtocolError("reasoning summary index out of range")
		}
		for len(item.parts) <= index {
			if reasoningPartsAlloc >= maxCollectedStreamReasoningParts {
				return nil, errCollectedStreamResponseTooLarge
			}
			if err := reserve(collectedReasoningPartOverhead); err != nil {
				return nil, err
			}
			item.parts = append(item.parts, &collectedReasoningPart{})
			reasoningPartsAlloc++
		}
		item.structured = true
		return item.parts[index], nil
	}
	addStructuredReasoningText := func(item *collectedReasoning, index int, text string) error {
		if text == "" {
			return nil
		}
		part, err := structuredPart(item, index)
		if err != nil {
			return err
		}
		if err := reserve(len(text)); err != nil {
			return err
		}
		part.text.WriteString(text)
		return nil
	}
	// applyLegacyReasoningDone keeps the arrival-order prefix rule for an
	// unindexed snapshot; the snapshot projection is already in part order.
	applyLegacyReasoningDone := func(item *collectedReasoning, rr *ir.ResponsesReasoning) error {
		if rr == nil {
			return nil
		}
		if len(rr.Summary) > maxCollectedStreamReasoningParts {
			return errCollectedStreamResponseTooLarge
		}
		want := rr.ReadableText()
		retained := item.text.String()
		switch {
		case retained == want:
		case strings.HasPrefix(want, retained):
			if err := addReasoningText(item, want[len(retained):]); err != nil {
				return err
			}
		default:
			return ir.ProtocolError("conflicting reasoning summary")
		}
		if item.seen != nil && item.seen.Equal(rr) {
			return nil
		}
		if len(rr.Summary) > item.legacyParts {
			if err := reserve((len(rr.Summary) - item.legacyParts) * collectedReasoningPartOverhead); err != nil {
				return err
			}
			item.legacyParts = len(rr.Summary)
		}
		if err := reserve(reasoningMetadataBytes(rr) - reasoningMetadataBytes(item.seen)); err != nil {
			return err
		}
		// One charged clone is shared by the dedup check and the returned block.
		// Keeping two copies would retain uncharged duplicate bytes.
		item.seen = rr.Clone()
		resp.Content[item.blockIdx].ResponsesReasoning = item.seen
		item.complete = true
		return nil
	}
	// applyStructuredReasoningDone reconciles a complete snapshot with the
	// per-index parts retained from deltas. Every streamed part must be a prefix
	// of the snapshot part at the same index, so a shrink or changed text fails.
	// The canonical ordered snapshot replaces the collected parts for encoding.
	applyStructuredReasoningDone := func(item *collectedReasoning, rr *ir.ResponsesReasoning) error {
		if rr == nil {
			return nil
		}
		if len(rr.Summary) > maxCollectedStreamReasoningParts {
			return errCollectedStreamResponseTooLarge
		}
		if len(item.parts) > len(rr.Summary) {
			return ir.ProtocolError("conflicting reasoning summary")
		}
		// An accepted snapshot may only grow. A later snapshot that shortens the
		// canonical summary or rewrites an accepted part is a conflict even when
		// no delta ever allocated that part.
		if item.seen != nil {
			if len(rr.Summary) < len(item.seen.Summary) {
				return ir.ProtocolError("conflicting reasoning summary")
			}
			for i, prev := range item.seen.Summary {
				snap := rr.Summary[i]
				if prev.Type != "" && snap.Type != "" && prev.Type != snap.Type {
					return ir.ProtocolError("conflicting reasoning summary")
				}
				if !strings.HasPrefix(snap.Text, prev.Text) {
					return ir.ProtocolError("conflicting reasoning summary")
				}
			}
		}
		for i, part := range item.parts {
			if part == nil {
				continue
			}
			snap := rr.Summary[i]
			// part.typ aliases the snapshot Type backing and is charged once as
			// part of the retained clone below; it is not a second allocation.
			if part.typ == "" {
				part.typ = snap.Type
			} else if snap.Type != "" && part.typ != snap.Type {
				return ir.ProtocolError("conflicting reasoning summary")
			}
			if part.text.Len() == 0 {
				continue
			}
			retained := part.text.String()
			switch {
			case retained == snap.Text:
			case strings.HasPrefix(snap.Text, retained):
				suffix := snap.Text[len(retained):]
				if err := reserve(len(suffix)); err != nil {
					return err
				}
				part.text.WriteString(suffix)
			default:
				return ir.ProtocolError("conflicting reasoning summary")
			}
		}
		if item.seen != nil && item.seen.Equal(rr) {
			return nil
		}
		// The retained clone is a distinct allocation from the live per-index
		// parts: charge one slice slot and part struct for every summary part
		// before cloning, so a final-only flood of empty parts cannot bypass the
		// budget.
		if err := reserve(reasoningSnapshotBytes(rr) - reasoningSnapshotBytes(item.seen)); err != nil {
			return err
		}
		item.seen = rr.Clone()
		resp.Content[item.blockIdx].ResponsesReasoning = item.seen
		item.structured = true
		item.complete = true
		return nil
	}
	// reasoningProjectionLen is the readable byte count of the projection that
	// will be retained on the block. It sums existing string lengths and does
	// not build the copy. Opaque metadata is excluded.
	reasoningProjectionLen := func(item *collectedReasoning) int {
		if item.seen != nil {
			n := 0
			for _, part := range item.seen.Summary {
				n += len(part.Text)
			}
			return n
		}
		if !item.structured {
			return item.text.Len()
		}
		n := 0
		for _, part := range item.parts {
			if part == nil {
				continue
			}
			n += part.text.Len()
		}
		return n
	}
	// collectedReasoningText returns one new charged readable projection. A
	// complete snapshot is canonical. Structured parts join in summary-index
	// order. Legacy bytes stay in arrival order. The caller reserves n before
	// this builds the copy. Snapshot strings and live part strings stay charged
	// independently and are not aliased by the returned string.
	collectedReasoningText := func(item *collectedReasoning, n int) string {
		if n == 0 {
			return ""
		}
		if item.seen != nil {
			var b strings.Builder
			b.Grow(n)
			for _, part := range item.seen.Summary {
				b.WriteString(part.Text)
			}
			return b.String()
		}
		if !item.structured {
			var b strings.Builder
			b.Grow(n)
			b.WriteString(item.text.String())
			return b.String()
		}
		var b strings.Builder
		b.Grow(n)
		for _, part := range item.parts {
			if part == nil {
				continue
			}
			b.WriteString(part.text.String())
		}
		return b.String()
	}
	type toolBuf struct {
		blockIdx int
		id       string
		name     string
		args     strings.Builder
	}
	servers := map[int]*collectedServerBuf{}
	serverStoppedThrough := -1
	serverFor := func(index int, typ, start string) (*collectedServerBuf, error) {
		if index < 0 {
			return nil, ir.ProtocolError("negative content block index")
		}
		if index <= serverStoppedThrough {
			return nil, ir.ProtocolError("server block restarted after stop")
		}
		if servers[index] != nil {
			return nil, ir.ProtocolError("server block started twice")
		}
		if len(servers) >= maxCollectedStreamBlocks {
			return nil, errCollectedStreamResponseTooLarge
		}
		sb := &collectedServerBuf{typ: typ, start: start}
		servers[index] = sb
		return sb, nil
	}
	tools := map[int]*toolBuf{}
	newTool := func(index int) (*toolBuf, error) {
		if tb := tools[index]; tb != nil {
			return tb, nil
		}
		if len(tools) >= maxTools {
			return nil, errCollectedStreamTooManyTools
		}
		flushText()
		resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockToolUse})
		tb := &toolBuf{blockIdx: len(resp.Content) - 1}
		tools[index] = tb
		return tb, nil
	}
	sawEvent := false
	decode := backend.decodeStream
	switch {
	case backend.decodeStreamDuplex != nil:
		decode = func(r io.Reader, emit func(ir.StreamEvent) error) error {
			if backend.protocol == domain.ProtocolCursor {
				return cursor.DecodeAgentStreamTools(clientTools, r, writeFrame, emit)
			}
			return backend.decodeStreamDuplex(r, writeFrame, emit)
		}
	case backend.protocol == domain.ProtocolKiro:
		decode = func(r io.Reader, emit func(ir.StreamEvent) error) error {
			return kiro.DecodeStreamToolsTransport(clientTools, r, emit, backend.id == "kiro-runtime")
		}
	}
	if decode == nil {
		return nil, fmt.Errorf("backend has no stream decoder")
	}
	err := decode(r, func(ev ir.StreamEvent) error {
		sawEvent = true
		switch ev.Kind {
		case ir.EventMessageStart:
			if ev.ID != "" {
				if err := replace(&resp.ID, ev.ID); err != nil {
					return err
				}
			}
			if ev.Model != "" {
				if err := replace(&resp.Model, ev.Model); err != nil {
					return err
				}
			}
			if err := applyCollectedResponseState(resp, ev, reserve); err != nil {
				return err
			}
			applyCollectedUsage(&resp.Usage, ev)
		case ir.EventTextStart:
			if ev.Index < 0 {
				return ir.ProtocolError("negative text block index")
			}
			if _, exists := texts[ev.Index]; exists {
				return ir.ProtocolError("text block started twice")
			}
			st, err := ensureTextState(ev.Index)
			if err != nil {
				return err
			}
			if ev.Text != "" {
				if err := reserve(len(ev.Text)); err != nil {
					return err
				}
				st.text.WriteString(ev.Text)
			}
		case ir.EventTextEnd:
			flushTextState(ev.Index)
		case ir.EventTextDelta:
			index := -1
			if ev.TextIndexed {
				index = ev.Index
			}
			if err := appendText(index, ev.Text); err != nil {
				return err
			}
		case ir.EventTextCitation:
			index := -1
			if ev.TextIndexed {
				index = ev.Index
			} else if texts[-1] == nil && texts[ev.Index] != nil {
				// Older producers set only Index. Bind it only to a matching open
				// source block; a dangling citation must not join another block.
				index = ev.Index
			}
			if err := appendCitation(index, ev.Data); err != nil {
				return err
			}
		case ir.EventReasoningDelta:
			if ev.Indexed {
				if ev.Text == "" {
					break
				}
				b, err := extraFor(ev.Index)
				if err != nil {
					return err
				}
				if err := reserve(len(ev.Text)); err != nil {
					return err
				}
				b.text.WriteString(ev.Text)
				break
			}
			if ev.ReasoningID != "" || ev.ReasoningIndexSet {
				item, err := reasoningFor(ev)
				if err != nil {
					return err
				}
				if err := addStructuredReasoningText(item, ev.SummaryIndex, ev.Text); err != nil {
					return err
				}
				break
			}
			// Legacy unindexed deltas keep the old empty-event behavior: an
			// empty fragment does not create a block.
			if ev.Text == "" {
				break
			}
			item, err := plainReasoningFor()
			if err != nil {
				return err
			}
			if err := addReasoningText(item, ev.Text); err != nil {
				return err
			}
		case ir.EventReasoningItemDone:
			if ev.ReasoningID != "" || ev.ReasoningIndexSet {
				item, err := reasoningFor(ev)
				if err != nil {
					return err
				}
				if err := applyStructuredReasoningDone(item, ev.Reasoning); err != nil {
					return err
				}
				break
			}
			item, err := plainReasoningFor()
			if err != nil {
				return err
			}
			if err := applyLegacyReasoningDone(item, ev.Reasoning); err != nil {
				return err
			}
		case ir.EventReasoningStart:
			if ev.Text == "" && ev.Signature == "" {
				break
			}
			b, err := extraFor(ev.Index)
			if err != nil {
				return err
			}
			if err := reserve(len(ev.Text) + len(ev.Signature)); err != nil {
				return err
			}
			b.text.WriteString(ev.Text)
			b.signature.WriteString(ev.Signature)
		case ir.EventReasoningSignature:
			if ev.Signature == "" {
				break
			}
			b, err := extraFor(ev.Index)
			if err != nil {
				return err
			}
			if err := reserve(len(ev.Signature)); err != nil {
				return err
			}
			b.signature.WriteString(ev.Signature)
		case ir.EventReasoningEnd:
			// The block stays in place; only its lifecycle ended.
		case ir.EventRedactedReasoning:
			if extraBlocks >= maxCollectedStreamBlocks {
				return errCollectedStreamResponseTooLarge
			}
			// The overhead is reserved with the payload so repeated empty
			// redacted events stop at the budget before appending.
			if err := reserve(collectedBlockOverhead + len(ev.Data)); err != nil {
				return err
			}
			flushText()
			resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockRedactedReasoning, RedactedData: ev.Data})
			extraBlocks++
		case ir.EventServerStart:
			if err := reserve(len(ev.Data) + len(ev.ServerType)); err != nil {
				return err
			}
			sb, err := serverFor(ev.Index, ev.ServerType, ev.Data)
			if err != nil {
				return err
			}
			_ = sb
		case ir.EventServerDelta:
			sb := servers[ev.Index]
			if sb == nil {
				return ir.ProtocolError("server delta without an open server block")
			}
			if err := reserve(len(ev.ArgsFrag)); err != nil {
				return err
			}
			sb.input.WriteString(ev.ArgsFrag)
		case ir.EventServerUpdate:
			sb := servers[ev.Index]
			if sb == nil {
				return ir.ProtocolError("server update without an open server block")
			}
			trimmed := []byte(ev.Data)
			if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
				return ir.ProtocolError("server update is malformed")
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(trimmed, &fields) != nil {
				return ir.ProtocolError("server update is malformed")
			}
			if err := reserve(len(trimmed)); err != nil {
				return err
			}
			if sb.updates == nil {
				sb.updates = map[string]json.RawMessage{}
			}
			for key, value := range fields {
				sb.updates[key] = append(json.RawMessage(nil), value...)
			}
		case ir.EventServerEnd:
			sb := servers[ev.Index]
			if sb == nil {
				return ir.ProtocolError("server stop without an open server block")
			}
			delete(servers, ev.Index)
			if ev.Index > serverStoppedThrough {
				serverStoppedThrough = ev.Index
			}
			raw, err := assembledServerBlock(sb)
			if err != nil {
				return err
			}
			if err := reserve(len(raw)); err != nil {
				return err
			}
			flushText()
			resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{WireType: sb.typ, Raw: raw}})
		case ir.EventServerBlock:
			if ev.Index < 0 {
				return ir.ProtocolError("negative content block index")
			}
			if ev.Index <= serverStoppedThrough {
				return ir.ProtocolError("server block restarted after stop")
			}
			raw := json.RawMessage(ev.Data)
			if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
				return ir.ProtocolError("server block is malformed")
			}
			if err := reserve(len(ev.Data)); err != nil {
				return err
			}
			flushText()
			resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{WireType: ev.ServerType, Raw: raw}})
			if ev.Index > serverStoppedThrough {
				serverStoppedThrough = ev.Index
			}
		case ir.EventToolCallStart:
			tb, err := newTool(ev.Index)
			if err != nil {
				return err
			}
			// Identity may arrive after argument fragments, or split across
			// repeated Starts. Update in place so retained args and already-
			// learned id/name survive; charge only the net identity change.
			id, name := tb.id, tb.name
			if ev.ToolID != "" {
				id = ev.ToolID
			}
			if ev.ToolName != "" {
				name = ev.ToolName
			}
			if err := reserve(len(id) + len(name) - len(tb.id) - len(tb.name)); err != nil {
				return err
			}
			tb.id = id
			tb.name = name
		case ir.EventToolCallDelta:
			tb, err := newTool(ev.Index)
			if err != nil {
				return err
			}
			if err := reserve(len(ev.ArgsFrag)); err != nil {
				return err
			}
			tb.args.WriteString(ev.ArgsFrag)
		case ir.EventFinish:
			// A reported finish replaces the whole usage object. Nonzero-only
			// collection would turn an explicit zero counter into a stale value.
			if ev.UsageReported {
				resp.Usage = ir.Usage{
					InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens,
					CacheReadTokens: ev.CacheReadTokens, CacheWriteTokens: ev.CacheWriteTokens,
					UsageReported: true,
				}.Clamped()
			} else {
				applyCollectedUsage(&resp.Usage, ev)
				resp.Usage.OutputTokens = ev.OutputTokens
			}
			if ev.StopReason != "" {
				resp.StopReason = ev.StopReason
			}
			resp.StopSequence = ev.StopSequence
			if err := applyCollectedResponseState(resp, ev, reserve); err != nil {
				return err
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
	flushText()
	for _, item := range reasoningItems {
		n := reasoningProjectionLen(item)
		// Reserve the returned projection before building it. Live part text and
		// the snapshot clone remain charged; this is the additional retained copy.
		if err := reserve(n); err != nil {
			return nil, err
		}
		resp.Content[item.blockIdx].Text = collectedReasoningText(item, n)
	}
	for _, b := range thinking {
		resp.Content[b.blockIdx].Text = b.text.String()
		resp.Content[b.blockIdx].AnthropicSignature = b.signature.String()
	}
	if len(servers) != 0 {
		return nil, ir.ProtocolError("server block closed by stream end")
	}
	for _, tb := range tools {
		args := []byte(tb.args.String())
		if len(args) == 0 || !json.Valid(args) {
			args = []byte("{}")
		}
		block := &resp.Content[tb.blockIdx]
		block.ToolID = tb.id
		block.ToolName = tb.name
		block.ToolInput = json.RawMessage(args)
	}
	return resp, nil
}

type collectedServerBuf struct {
	typ     string
	start   string
	input   strings.Builder
	updates map[string]json.RawMessage
}

func assembledServerBlock(sb *collectedServerBuf) (json.RawMessage, error) {
	if sb == nil || sb.start == "" || !json.Valid([]byte(sb.start)) {
		return nil, ir.ProtocolError("server block is malformed")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(sb.start), &obj) != nil || obj["type"] == nil {
		return nil, ir.ProtocolError("server block is malformed")
	}
	for key, value := range sb.updates {
		obj[key] = value
	}
	if sb.input.Len() > 0 {
		raw := []byte(sb.input.String())
		if !json.Valid(raw) {
			return nil, ir.ProtocolError("server input is malformed")
		}
		obj["input"] = json.RawMessage(raw)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// reasoningSnapshotBytes is the retained cost of the canonical snapshot clone:
// item metadata plus one slice slot and part struct per summary part. It is
// distinct from the live per-index parts, which are charged when they are
// allocated, so both retained allocations are billed.
func reasoningSnapshotBytes(rr *ir.ResponsesReasoning) int {
	if rr == nil {
		return 0
	}
	return reasoningMetadataBytes(rr) + len(rr.Summary)*collectedReasoningPartOverhead
}

// reasoningMetadataBytes is the retained string metadata footprint that is not
// part of the readable text projection: item ID, status, encrypted bytes, and
// every summary type and text copy. The per-part slice and struct bookkeeping is
// added by reasoningSnapshotBytes for the structured clone, and charged
// separately through legacyParts for the legacy path.
func reasoningMetadataBytes(rr *ir.ResponsesReasoning) int {
	if rr == nil {
		return 0
	}
	n := len(rr.ID) + len(rr.Status)
	if rr.EncryptedContent != nil {
		n += len(*rr.EncryptedContent)
	}
	for _, p := range rr.Summary {
		n += len(p.Type) + len(p.Text)
	}
	return n
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

// upstreamErrorMessage extracts recognized structured messages. Unstructured
// bodies are intentionally excluded from client envelopes and request history.
func upstreamErrorMessage(status int, body []byte) string {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
				return nested.Message
			}
			var message string
			if json.Unmarshal(envelope.Error, &message) == nil && message != "" {
				return message
			}
		}
		if envelope.Message != "" {
			return envelope.Message
		}
		if envelope.Detail != "" {
			return envelope.Detail
		}
	}
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("upstream returned %d %s", status, text)
	}
	return fmt.Sprintf("upstream returned HTTP status %d", status)
}

// recordLog persists a request log fire-and-forget so a slow DB write never
// blocks the response. It runs on a fresh context since the request's may be
// done by the time the write lands.
func (p *Proxy) recordLog(ctx context.Context, l *domain.RequestLog) {
	logger := observability.Logger(ctx, p.logger)
	bounded := *l
	bounded.ErrMsg = clampErrorMessage(bounded.ErrMsg)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.store.CreateRequestLog(ctx, &bounded); err != nil {
			logger.Error("request_log_write_failed",
				"event", "request_log_write_failed",
				"error", err,
			)
		}
	}()
}

// estimateUsage approximates tokens from character length when the upstream
// sent no usage. Input includes the request text, tool schemas, and tool
// results. Output includes assistant text and tool-call names and arguments.
// Four characters is one token, matching 9router. The result is an estimate,
// not Cursor billed usage.
func estimateUsage(req *ir.Request, resp *ir.Response) (int, int) {
	if req == nil && resp == nil {
		return 0, 0
	}
	var in, out strings.Builder
	if req != nil {
		in.WriteString(req.System)
		for _, msg := range req.Messages {
			writeUsageBlocks(&in, msg.Content)
		}
		for _, tool := range req.Tools {
			in.WriteString(tool.Name)
			in.WriteString(tool.Description)
			in.Write(tool.Parameters)
		}
	}
	if resp != nil {
		writeUsageBlocks(&out, resp.Content)
	}
	return charsToTokens(in.Len()), charsToTokens(out.Len())
}

func writeUsageBlocks(b *strings.Builder, blocks []ir.ContentBlock) {
	for _, block := range blocks {
		switch block.Type {
		case ir.BlockText, ir.BlockReasoning:
			// Readable text only. AnthropicSignature and RedactedData are opaque
			// and must not inflate a character-length estimate.
			b.WriteString(block.Text)
		case ir.BlockToolUse:
			b.WriteString(block.ToolName)
			b.Write(block.ToolInput)
		case ir.BlockToolResult:
			writeUsageBlocks(b, block.ToolResult)
		}
	}
}

// applyCursorUsage keeps a normal turn_ended when UsageReported is set.
// Inclusive input may exceed the request body because Cursor adds its own
// context; that count is not rejected. Explicit zero stays reported and is
// not an estimate. Missing or incomplete counters, including an immediate
// MCP handoff, fall back to a character estimate with cache buckets at zero.
// The estimate uses the same 4 characters per token as 9router and does not
// add 9router's context-guard buffer.
func applyCursorUsage(req *ir.Request, resp *ir.Response, requestBytes int) {
	if resp == nil {
		return
	}
	if resp.Usage.UsageReported {
		resp.Usage = resp.Usage.Clamped()
		return
	}
	inEst, outEst := estimateUsage(req, resp)
	if inEst == 0 && requestBytes > 0 {
		inEst = charsToTokens(requestBytes)
	}
	resp.Usage = ir.Usage{InputTokens: inEst, OutputTokens: outEst}
}

// usageInputPlausible reports whether an upstream input-token count can
// fit the request bytes. Zero is plausible: the upstream may omit usage.
// The bound is one token per two bytes, which leaves room for a hidden
// prompt without accepting a multi-million count from a sub-megabyte body.
func usageInputPlausible(requestBytes, inputTokens int) bool {
	if inputTokens <= 0 {
		return true
	}
	if requestBytes < 0 {
		requestBytes = 0
	}
	return inputTokens <= (requestBytes/2)+4096
}

func charsToTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + usageEstimateCharsPerToken - 1) / usageEstimateCharsPerToken
}

func writeErr(w http.ResponseWriter, c codec, status int, message, errType string) {
	message = clampErrorMessage(message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(c.encodeError(message, errType))
}

// authResult is the display label plus a uniqueness-safe tenant scope.
// Name is the access-key label (or "(open)") for request logs. Scope is the
// stored key hash, never the display name; open mode uses openTenantScope.
type authResult struct {
	Name  string
	Scope string
}

// authenticate verifies the bearer/x-api-key token against stored access keys.
// When no access keys exist the proxy runs in open mode and accepts every
// request unauthenticated.
func (p *Proxy) authenticate(r *http.Request) (authResult, bool) {
	token := extractToken(r)
	if token != "" {
		if key, err := p.store.VerifyToken(r.Context(), token); err == nil {
			return authResult{Name: key.Name, Scope: key.Hash}, true
		}
	}
	// No valid token: allow only when there are no keys configured at all.
	if n, err := p.store.CountAccessKeys(r.Context()); err == nil && n == 0 {
		return authResult{Name: "(open)", Scope: openTenantScope}, true
	}
	return authResult{}, false
}

func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
