// Package proxy implements the bidirectional inference proxy: it accepts
// requests on an OpenAI or Anthropic ingress endpoint, resolves the requested
// combo to an upstream provider, translates the payload to the provider's
// protocol when they differ, forwards it, and translates the response back.
package proxy

import (
	"io"
	"net/http"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
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
type codec struct {
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
}

func New(s *store.Store) *Proxy {
	return &Proxy{
		store:        s,
		client:       &http.Client{Timeout: 5 * time.Minute},
		streamClient: &http.Client{},
	}
}

// Mount registers the proxy ingress endpoints.
func (p *Proxy) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		p.serve(w, r, openaiCodec)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		p.serve(w, r, anthropicCodec)
	})
}
