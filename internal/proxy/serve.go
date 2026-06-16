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
	provider := combo.Provider
	rec.Provider = provider.Name
	rec.UpstreamModel = combo.UpstreamModel
	backend := backendCodec(provider.Protocol)

	if ingress.id == backend.id {
		if meta.Stream {
			p.streamPassthrough(w, r.Context(), res, ingress, provider, combo.UpstreamModel, body, r.Header)
		} else {
			p.servePassthrough(w, r.Context(), res, ingress, provider, combo.UpstreamModel, body, r.Header)
		}
		return
	}
	if meta.Stream {
		p.streamTranslated(w, r.Context(), res, ingress, backend, provider, combo.UpstreamModel, body)
	} else {
		p.serveTranslated(w, r.Context(), res, ingress, backend, provider, combo.UpstreamModel, body)
	}
}

// servePassthrough forwards the body unchanged except for the model rewrite,
// preserving any provider-specific fields the IR does not model. The upstream
// response is relayed as-is since its format already matches the ingress.
func (p *Proxy) servePassthrough(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, upstreamModel string, body []byte, clientHeaders http.Header) {
	rewritten, err := rewriteModel(body, upstreamModel)
	if err != nil {
		res.fail(w, ingress, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}

	status, respBody, err := p.forward(ctx, provider, ingress.upstreamPath, rewritten, clientHeaders)
	if err != nil {
		res.fail(w, ingress, http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
		return
	}
	res.status = status
	// Usage is not modeled in passthrough; recover it best-effort from the raw
	// body, which already matches the ingress format.
	res.inTok, res.outTok = parseUsage(respBody)
	if status < 200 || status >= 300 {
		p.debugf("passthrough %s %s: upstream %d\nresponse: %s", ingress.id, ingress.upstreamPath, status, respBody)
		res.errMsg = upstreamErrorMessage(respBody)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

// serveTranslated converts the request to the backend protocol, forwards it, and
// converts the response back to the ingress protocol.
func (p *Proxy) serveTranslated(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, body []byte) {
	req, err := ingress.decodeRequest(body)
	if err != nil {
		res.fail(w, ingress, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	req.Model = upstreamModel
	req.Stream = false

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		res.fail(w, ingress, http.StatusInternalServerError, "failed to encode upstream request", "api_error")
		return
	}

	status, respBody, err := p.forward(ctx, provider, backend.upstreamPath, upstreamBody, nil)
	if err != nil {
		res.fail(w, ingress, http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
		return
	}
	if status < 200 || status >= 300 {
		// Surface the upstream error message in the ingress error envelope.
		p.debugf("translate %s -> %s %s: upstream %d\nrequest: %s\nresponse: %s",
			ingress.id, backend.id, backend.upstreamPath, status, upstreamBody, respBody)
		res.fail(w, ingress, status, upstreamErrorMessage(respBody), "api_error")
		return
	}

	resp, err := backend.decodeResponse(respBody)
	if err != nil {
		p.debugf("translate %s -> %s: decode response failed: %v\nresponse: %s",
			ingress.id, backend.id, err, respBody)
		res.fail(w, ingress, http.StatusBadGateway, "failed to decode upstream response", "api_error")
		return
	}
	res.inTok = resp.Usage.InputTokens
	res.outTok = resp.Usage.OutputTokens
	out, err := ingress.encodeResponse(resp)
	if err != nil {
		res.fail(w, ingress, http.StatusInternalServerError, "failed to encode response", "api_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
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
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &u) != nil {
		return 0, 0
	}
	return u.Usage.PromptTokens + u.Usage.InputTokens, u.Usage.CompletionTokens + u.Usage.OutputTokens
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
