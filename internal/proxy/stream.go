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
// names) so each is flushed to the client immediately. It returns an
// attemptResult so the resolution loop can fail over to the next target on a
// pre-commit failure; once the 200 header is written the response is committed.
func (p *Proxy) streamPassthrough(w http.ResponseWriter, ctx context.Context, res *reqResult, ingress codec, provider *domain.Provider, upstreamModel string, body []byte, clientHeaders http.Header) attemptResult {
	rewritten, err := rewriteModel(body, upstreamModel)
	if err != nil {
		return terminal(http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
	}
	resp, err := p.forwardStream(ctx, provider, ingress.upstreamPath, rewritten, clientHeaders)
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		p.debugf("stream passthrough %s %s: upstream %d\nresponse: %s", ingress.id, ingress.upstreamPath, resp.StatusCode, errBody)
		return retryable(resp.StatusCode, upstreamErrorMessage(errBody), "api_error")
	}

	sw, ok := sse.NewWriter(w)
	if !ok {
		return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
	}
	// Streaming passthrough relays raw events; token usage is not parsed out of
	// the relayed SSE, so it stays at 0 in the log.
	res.status = http.StatusOK
	w.WriteHeader(http.StatusOK)
	reader := sse.NewReader(resp.Body)
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			return committed()
		}
		if err != nil {
			log.Printf("stream passthrough read: %v", err)
			return committed()
		}
		if err := sw.WriteEvent(ev.Name, ev.Data); err != nil {
			return committed() // client disconnected
		}
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
	req.Model = upstreamModel
	req.Stream = true

	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		return terminal(http.StatusInternalServerError, "failed to encode upstream request", "api_error")
	}
	resp, err := p.forwardStream(ctx, provider, backend.upstreamPath, upstreamBody, nil)
	if err != nil {
		return retryable(http.StatusBadGateway, "upstream request failed: "+err.Error(), "api_error")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		p.debugf("stream translate %s -> %s %s: upstream %d\nrequest: %s\nresponse: %s",
			ingress.id, backend.id, backend.upstreamPath, resp.StatusCode, upstreamBody, errBody)
		return retryable(resp.StatusCode, upstreamErrorMessage(errBody), "api_error")
	}

	sw, ok := sse.NewWriter(w)
	if !ok {
		return terminal(http.StatusInternalServerError, "streaming unsupported by server", "api_error")
	}
	res.status = http.StatusOK
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
		return committed()
	}
	if err := enc.Close(sw); err != nil {
		log.Printf("stream translate close: %v", err)
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
