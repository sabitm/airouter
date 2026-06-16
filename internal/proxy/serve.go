package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/store"
)

const maxBodyBytes = 16 << 20 // 16 MiB ceiling on inbound request bodies

// reqResult accumulates the outcome of one request for logging. Each serve path
// fills it in; serve records a RequestLog once on completion.
type reqResult struct {
	status int
	inTok  int
	outTok int
	errMsg string
}

// fail writes an error envelope and records the failure on the result.
func (res *reqResult) fail(w http.ResponseWriter, ingress codec, status int, message, errType string) {
	res.status = status
	res.errMsg = message
	writeErr(w, ingress, status, message, errType)
}

// attemptResult reports the outcome of trying one combo target.
//
//   - written:  a response was committed to the client (success, or a stream
//     that began). The resolution loop must not write anything further.
//   - retry:    the attempt failed before committing; the loop may try the next
//     target. status/errMsg/errType describe the failure for a possible envelope.
//
// A terminal pre-commit error (bad request body, encode failure) returns neither
// written nor retry: the loop stops and surfaces the envelope once.
type attemptResult struct {
	written bool
	retry   bool
	status  int
	errMsg  string
	errType string
}

func committed() attemptResult { return attemptResult{written: true} }

func retryable(status int, message, errType string) attemptResult {
	return attemptResult{retry: true, status: status, errMsg: message, errType: errType}
}

func terminal(status int, message, errType string) attemptResult {
	return attemptResult{status: status, errMsg: message, errType: errType}
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
		// Surface the client-facing failure reason in the terminal. Upstream
		// exchanges log their own detail; this covers pre-upstream rejections
		// (auth, unknown combo, bad body) that otherwise leave only an access-log
		// status with no reason.
		if res.errMsg != "" {
			p.debugf("request failed: %s %s %d: %s", ingress.id, r.Method, res.status, res.errMsg)
		}
		p.recordLog(rec)
	}()

	keyName, ok := p.authenticate(r)
	if !ok {
		res.fail(w, ingress, http.StatusUnauthorized, "invalid or missing access key", "authentication_error")
		return
	}
	rec.AccessKeyName = keyName

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
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
	candidates := p.orderTargets(combo)
	if len(candidates) == 0 {
		res.fail(w, ingress, http.StatusInternalServerError, "combo has no targets: "+meta.Model, "api_error")
		return
	}

	// Walk the ordered targets. A target that fails before any byte reaches the
	// client falls through to the next; the first that commits a response ends
	// the walk. If all fail, the last failure's envelope is written below.
	var last attemptResult
	for i, t := range candidates {
		provider := t.Provider
		rec.Provider = provider.Name
		rec.UpstreamModel = t.UpstreamModel
		backend := backendCodec(provider.Protocol)

		if ingress.id == backend.id {
			if meta.Stream {
				last = p.streamPassthrough(w, r.Context(), res, ingress, provider, t.UpstreamModel, body, r.Header)
			} else {
				last = p.servePassthrough(w, r.Context(), res, ingress, provider, t.UpstreamModel, body, r.Header)
			}
		} else if meta.Stream {
			last = p.streamTranslated(w, r.Context(), res, ingress, backend, provider, t.UpstreamModel, body)
		} else {
			last = p.serveTranslated(w, r.Context(), res, ingress, backend, provider, t.UpstreamModel, body)
		}

		if !last.retry {
			break
		}
		if i < len(candidates)-1 {
			p.debugf("combo %s: target %d (%s) failed: %d %s; advancing",
				combo.Name, i, provider.Name, last.status, last.errMsg)
		}
	}

	// last.written is true when a response was committed (success or a stream that
	// began). Otherwise either every target was exhausted (last.retry) or a
	// terminal pre-commit error occurred; surface its envelope.
	if !last.written {
		status := last.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		res.fail(w, ingress, status, last.errMsg, last.errType)
	}
}

// orderTargets returns the combo's targets in the order the resolution loop
// should try them. Failover keeps position order; round-robin rotates the start
// by a per-combo counter, then continues through the remainder so it still fails
// over past a dead target.
func (p *Proxy) orderTargets(combo *domain.Combo) []domain.ComboTarget {
	targets := combo.Targets
	if combo.Strategy != domain.StrategyRoundRobin || len(targets) <= 1 {
		return targets
	}
	start := p.nextRoundRobin(combo.ID, len(targets))
	out := make([]domain.ComboTarget, 0, len(targets))
	for i := range targets {
		out = append(out, targets[(start+i)%len(targets)])
	}
	return out
}

// servePassthrough forwards the body unchanged except for the model rewrite,
// preserving any provider-specific fields the IR does not model. The upstream
// response is relayed as-is since its format already matches the ingress.
func (p *Proxy) servePassthrough(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, upstreamModel string, body []byte, clientHeaders http.Header) attemptResult {
	rewritten, err := rewriteModel(body, upstreamModel)
	if err != nil {
		return terminal(http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
	}

	status, respBody, err := p.forward(ctx, provider, ingress.upstreamPath, rewritten, clientHeaders)
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	if status < 200 || status >= 300 {
		p.debugf("passthrough %s %s: upstream %d\nresponse: %s", ingress.id, ingress.upstreamPath, status, respBody)
		return retryable(status, upstreamErrorMessage(respBody), "api_error")
	}
	res.status = status
	// Usage is not modeled in passthrough; recover it best-effort from the raw
	// body, which already matches the ingress format.
	res.inTok, res.outTok = parseUsage(respBody)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
	return committed()
}

// serveTranslated converts the request to the backend protocol, forwards it, and
// converts the response back to the ingress protocol.
func (p *Proxy) serveTranslated(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, body []byte) attemptResult {
	req, err := ingress.decodeRequest(body)
	if err != nil {
		return terminal(http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	req.Model = upstreamModel
	req.Stream = false

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to encode upstream request", "api_error")
	}

	status, respBody, err := p.forward(ctx, provider, backend.upstreamPath, upstreamBody, nil)
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	if status < 200 || status >= 300 {
		// Surface the upstream error message in the ingress error envelope.
		p.debugf("translate %s -> %s %s: upstream %d\nrequest: %s\nresponse: %s",
			ingress.id, backend.id, backend.upstreamPath, status, upstreamBody, respBody)
		return retryable(status, upstreamErrorMessage(respBody), "api_error")
	}

	resp, err := backend.decodeResponse(respBody)
	if err != nil {
		p.debugf("translate %s -> %s: decode response failed: %v\nresponse: %s",
			ingress.id, backend.id, err, respBody)
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

// parseUsage recovers token counts from a unary response body without knowing
// its format: OpenAI uses prompt_tokens/completion_tokens, Anthropic uses
// input_tokens/output_tokens. Returns 0,0 when no usage object is present.
func parseUsage(body []byte) (in, out int) {
	var u struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			// Anthropic reports cached input separately; fold it in so the count
			// reflects total input regardless of cache state.
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &u) != nil {
		return 0, 0
	}
	in = u.Usage.PromptTokens + u.Usage.InputTokens + u.Usage.CacheCreationInputTokens + u.Usage.CacheReadInputTokens
	out = u.Usage.CompletionTokens + u.Usage.OutputTokens
	return in, out
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

// debugf logs an upstream error exchange when debug mode is on. No-op otherwise.
func (p *Proxy) debugf(format string, args ...any) {
	if p.debug {
		log.Printf("[debug] "+format, args...)
	}
}

// recordLog persists a request log fire-and-forget so a slow DB write never
// blocks the response. It runs on a fresh context since the request's may be
// done by the time the write lands.
func (p *Proxy) recordLog(l *domain.RequestLog) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.store.CreateRequestLog(ctx, l); err != nil {
			log.Printf("request log: %v", err)
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
