package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/store"
)

const maxBodyBytes = 16 << 20 // 16 MiB ceiling on inbound request bodies

// serve runs the full ingress lifecycle for one request. ingress is the codec
// for the endpoint the client called.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, ingress codec) {
	if !p.authenticate(r) {
		writeErr(w, ingress, http.StatusUnauthorized, "invalid or missing access key", "authentication_error")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, ingress, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}

	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		writeErr(w, ingress, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if meta.Model == "" {
		writeErr(w, ingress, http.StatusBadRequest, "missing 'model' field", "invalid_request_error")
		return
	}
	// Streaming translation lands in phase 3; reject explicitly so SSE clients
	// fail fast rather than receiving a unary body they cannot parse.
	if meta.Stream {
		writeErr(w, ingress, http.StatusNotImplemented, "streaming is not supported yet", "not_supported_error")
		return
	}

	combo, err := p.store.GetComboByName(r.Context(), meta.Model)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, ingress, http.StatusNotFound, "unknown model (combo): "+meta.Model, "invalid_request_error")
			return
		}
		writeErr(w, ingress, http.StatusInternalServerError, "combo lookup failed", "api_error")
		return
	}
	provider := combo.Provider
	backend := backendCodec(provider.Protocol)

	if ingress.protocol == backend.protocol {
		p.servePassthrough(w, r.Context(), ingress, provider, combo.UpstreamModel, body)
		return
	}
	p.serveTranslated(w, r.Context(), ingress, backend, provider, combo.UpstreamModel, body)
}

// servePassthrough forwards the body unchanged except for the model rewrite,
// preserving any provider-specific fields the IR does not model. The upstream
// response is relayed as-is since its format already matches the ingress.
func (p *Proxy) servePassthrough(w http.ResponseWriter, ctx context.Context, ingress codec, provider *domain.Provider, upstreamModel string, body []byte) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		writeErr(w, ingress, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	generic["model"], _ = json.Marshal(upstreamModel)
	rewritten, err := json.Marshal(generic)
	if err != nil {
		writeErr(w, ingress, http.StatusInternalServerError, "failed to rewrite request", "api_error")
		return
	}

	status, respBody, err := p.forward(ctx, provider, ingress.upstreamPath, rewritten)
	if err != nil {
		writeErr(w, ingress, http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

// serveTranslated converts the request to the backend protocol, forwards it, and
// converts the response back to the ingress protocol.
func (p *Proxy) serveTranslated(w http.ResponseWriter, ctx context.Context, ingress, backend codec, provider *domain.Provider, upstreamModel string, body []byte) {
	req, err := ingress.decodeRequest(body)
	if err != nil {
		writeErr(w, ingress, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	req.Model = upstreamModel
	req.Stream = false

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		writeErr(w, ingress, http.StatusInternalServerError, "failed to encode upstream request", "api_error")
		return
	}

	status, respBody, err := p.forward(ctx, provider, backend.upstreamPath, upstreamBody)
	if err != nil {
		writeErr(w, ingress, http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
		return
	}
	if status < 200 || status >= 300 {
		// Surface the upstream error message in the ingress error envelope.
		writeErr(w, ingress, status, upstreamErrorMessage(respBody), "api_error")
		return
	}

	resp, err := backend.decodeResponse(respBody)
	if err != nil {
		writeErr(w, ingress, http.StatusBadGateway, "failed to decode upstream response", "api_error")
		return
	}
	out, err := ingress.encodeResponse(resp)
	if err != nil {
		writeErr(w, ingress, http.StatusInternalServerError, "failed to encode response", "api_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
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

func writeErr(w http.ResponseWriter, c codec, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(c.encodeError(message, errType))
}

// authenticate verifies the bearer/x-api-key token against stored access keys.
func (p *Proxy) authenticate(r *http.Request) bool {
	token := extractToken(r)
	if token == "" {
		return false
	}
	_, err := p.store.VerifyToken(r.Context(), token)
	return err == nil
}

func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
