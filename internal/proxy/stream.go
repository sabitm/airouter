package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// streamPassthrough relays an upstream SSE response of the same protocol as the
// ingress, rewriting only the request model. Events are re-emitted (preserving
// names) so each is flushed to the client immediately.
func (p *Proxy) streamPassthrough(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, upstreamModel string, body []byte, clientHeaders http.Header) {
	rewritten, err := rewriteModel(body, upstreamModel)
	if err != nil {
		res.fail(w, ingress, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	resp, err := p.forwardStream(ctx, provider, ingress.upstreamPath, rewritten, clientHeaders)
	if err != nil {
		res.fail(w, ingress, http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		p.debugf("stream passthrough %s %s: upstream %d\nresponse: %s", ingress.id, ingress.upstreamPath, resp.StatusCode, errBody)
		res.fail(w, ingress, resp.StatusCode, upstreamErrorMessage(errBody), "api_error")
		return
	}

	sw, ok := sse.NewWriter(w)
	if !ok {
		res.fail(w, ingress, http.StatusInternalServerError, "streaming unsupported by server", "api_error")
		return
	}
	// Streaming passthrough relays raw events; token usage is not parsed out of
	// the relayed SSE, so it stays at 0 in the log.
	w.WriteHeader(http.StatusOK)
	reader := sse.NewReader(resp.Body)
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Printf("stream passthrough read: %v", err)
			return
		}
		if err := sw.WriteEvent(ev.Name, ev.Data); err != nil {
			return // client disconnected
		}
	}
}

// streamTranslated converts an ingress streaming request to the backend
// protocol, then pumps backend SSE events through the IR into ingress-format
// SSE. Errors before the first byte fall back to a unary error envelope; errors
// mid-stream simply terminate the response.
func (p *Proxy) streamTranslated(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress, backend codec, provider *domain.Provider, upstreamModel string, body []byte) {
	req, err := ingress.decodeRequest(body)
	if err != nil {
		res.fail(w, ingress, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	req.Model = upstreamModel
	req.Stream = true

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		res.fail(w, ingress, http.StatusInternalServerError, "failed to encode upstream request", "api_error")
		return
	}
	resp, err := p.forwardStream(ctx, provider, backend.upstreamPath, upstreamBody, nil)
	if err != nil {
		res.fail(w, ingress, http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		p.debugf("stream translate %s -> %s %s: upstream %d\nrequest: %s\nresponse: %s",
			ingress.id, backend.id, backend.upstreamPath, resp.StatusCode, upstreamBody, errBody)
		res.fail(w, ingress, resp.StatusCode, upstreamErrorMessage(errBody), "api_error")
		return
	}

	sw, ok := sse.NewWriter(w)
	if !ok {
		res.fail(w, ingress, http.StatusInternalServerError, "streaming unsupported by server", "api_error")
		return
	}
	w.WriteHeader(http.StatusOK)

	enc := ingress.newStreamEncoder(upstreamModel)
	err = backend.decodeStream(resp.Body, func(ev ir.StreamEvent) error {
		// Token counts arrive on distinct events: input at message start, output
		// at finish.
		switch ev.Kind {
		case ir.EventMessageStart:
			res.inTok = ev.InputTokens
		case ir.EventFinish:
			res.outTok = ev.OutputTokens
		}
		return enc.Encode(ev, sw)
	})
	if err != nil {
		// Already streaming; cannot switch to a unary error. Stop cleanly.
		log.Printf("stream translate: %v", err)
		p.debugf("stream translate %s -> %s: mid-stream error: %v", ingress.id, backend.id, err)
		return
	}
	if err := enc.Close(sw); err != nil {
		log.Printf("stream translate close: %v", err)
	}
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
