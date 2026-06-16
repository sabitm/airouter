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
			log.Printf("stream passthrough read: %v", err)
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
	var u struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
		Message *struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &u) != nil {
		return
	}
	if u.Usage != nil {
		if in := u.Usage.PromptTokens + u.Usage.InputTokens; in != 0 {
			res.inTok = in
		}
		if out := u.Usage.CompletionTokens + u.Usage.OutputTokens; out != 0 {
			res.outTok = out
		}
	}
	if u.Message != nil && u.Message.Usage != nil {
		if u.Message.Usage.InputTokens != 0 {
			res.inTok = u.Message.Usage.InputTokens
		}
		if u.Message.Usage.OutputTokens != 0 {
			res.outTok = u.Message.Usage.OutputTokens
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
