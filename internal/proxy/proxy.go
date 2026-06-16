// Package proxy implements the bidirectional inference proxy: it accepts
// requests on an OpenAI or Anthropic ingress endpoint, resolves the requested
// combo to an upstream provider, translates the payload to the provider's
// protocol when they differ, forwards it, and translates the response back.
package proxy

import (
	"io"
	"net/http"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
	"airouter/internal/store"
)

// streamEncoder renders IR stream events into an ingress-format SSE stream.
type streamEncoder interface {
	Encode(ev ir.StreamEvent, w *sse.Writer) error
	Close(w *sse.Writer) error
}

// codec bundles the translation directions plus error rendering and the upstream
// request path for one wire format. It covers both unary and streaming.
//
// id identifies the wire format for the passthrough decision: a request passes
// through only when the ingress and backend ids match. protocol selects the
// backend codec from a provider's protocol. Ingress-only formats (responses)
// share a protocol with a backend but have a distinct id, so they never pass
// through and always translate.
type codec struct {
	id             string
	protocol       domain.Protocol
	decodeRequest  func([]byte) (*ir.Request, error)
	encodeRequest  func(*ir.Request) ([]byte, error)
	decodeResponse func([]byte) (*ir.Response, error)
	encodeResponse func(*ir.Response) ([]byte, error)
	encodeError    func(message, errType string) []byte
	upstreamPath   string // appended to the provider base URL when this is the backend

	decodeStream     func(io.Reader, func(ir.StreamEvent) error) error
	newStreamEncoder func(model string) streamEncoder
}

var openaiCodec = codec{
	id:               "oai-chat",
	protocol:         domain.ProtocolOpenAI,
	decodeRequest:    openai.DecodeRequest,
	encodeRequest:    openai.EncodeRequest,
	decodeResponse:   openai.DecodeResponse,
	encodeResponse:   openai.EncodeResponse,
	encodeError:      openai.EncodeError,
	upstreamPath:     "/chat/completions",
	decodeStream:     openai.DecodeStream,
	newStreamEncoder: func(model string) streamEncoder { return openai.NewStreamEncoder(model) },
}

var anthropicCodec = codec{
	id:               "anth-msg",
	protocol:         domain.ProtocolAnthropic,
	decodeRequest:    anthropic.DecodeRequest,
	encodeRequest:    anthropic.EncodeRequest,
	decodeResponse:   anthropic.DecodeResponse,
	encodeResponse:   anthropic.EncodeResponse,
	encodeError:      anthropic.EncodeError,
	upstreamPath:     "/messages",
	decodeStream:     anthropic.DecodeStream,
	newStreamEncoder: func(model string) streamEncoder { return anthropic.NewStreamEncoder(model) },
}

// responsesCodec is ingress-only: it has no backend directions (encodeRequest,
// decodeResponse, decodeStream, upstreamPath) because a provider is never
// reached over the Responses API. Its distinct id ensures it always translates.
var responsesCodec = codec{
	id:               "oai-responses",
	protocol:         domain.ProtocolOpenAI,
	decodeRequest:    responses.DecodeRequest,
	encodeResponse:   responses.EncodeResponse,
	encodeError:      responses.EncodeError,
	newStreamEncoder: func(model string) streamEncoder { return responses.NewStreamEncoder(model) },
}

func backendCodec(p domain.Protocol) codec {
	if p == domain.ProtocolAnthropic {
		return anthropicCodec
	}
	return openaiCodec
}

type Proxy struct {
	store *store.Store
	// client bounds unary requests; streamClient has no total timeout so long
	// SSE streams are governed by the request context instead.
	client       *http.Client
	streamClient *http.Client
	debug        bool

	// rr holds per-combo round-robin counters, keyed by combo id. In-memory only:
	// the rotation resets on restart, which is acceptable for load spreading.
	rrMu sync.Mutex
	rr   map[int64]uint64
}

func New(s *store.Store, debug bool) *Proxy {
	return &Proxy{
		store:        s,
		client:       &http.Client{Timeout: 5 * time.Minute},
		streamClient: &http.Client{},
		debug:        debug,
		rr:           map[int64]uint64{},
	}
}

// nextRoundRobin returns the starting target index for a round-robin combo with
// n targets, advancing the per-combo counter so successive requests rotate.
func (p *Proxy) nextRoundRobin(comboID int64, n int) int {
	if n <= 1 {
		return 0
	}
	p.rrMu.Lock()
	i := p.rr[comboID]
	p.rr[comboID] = i + 1
	p.rrMu.Unlock()
	return int(i % uint64(n))
}

// Mount registers the proxy ingress endpoints. Each is mounted under both the
// canonical /v1 prefix and a bare path: clients disagree on whether the base URL
// already includes /v1 (the Anthropic SDK hardcodes /v1/messages, while model
// discovery appends a bare /models), so accepting both spares the user from
// guessing which prefix to put in the provider URL.
func (p *Proxy) Mount(mux *http.ServeMux) {
	serve := func(c codec) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { p.serve(w, r, c) }
	}
	routes := []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"POST", "/chat/completions", serve(openaiCodec)},
		{"POST", "/messages", serve(anthropicCodec)},
		{"POST", "/responses", serve(responsesCodec)},
		{"GET", "/models", p.handleModels},
	}
	for _, rt := range routes {
		mux.HandleFunc(rt.method+" /v1"+rt.path, rt.handler)
		mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
	}
}
