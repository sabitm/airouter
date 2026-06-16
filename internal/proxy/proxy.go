// Package proxy implements the bidirectional inference proxy: it accepts
// requests on an OpenAI or Anthropic ingress endpoint, resolves the requested
// combo to an upstream provider, translates the payload to the provider's
// protocol when they differ, forwards it, and translates the response back.
package proxy

import (
	"net/http"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/store"
)

// codec bundles the four translation directions plus error rendering and the
// upstream request path for one wire format.
type codec struct {
	protocol       domain.Protocol
	decodeRequest  func([]byte) (*ir.Request, error)
	encodeRequest  func(*ir.Request) ([]byte, error)
	decodeResponse func([]byte) (*ir.Response, error)
	encodeResponse func(*ir.Response) ([]byte, error)
	encodeError    func(message, errType string) []byte
	upstreamPath   string // appended to the provider base URL when this is the backend
}

var openaiCodec = codec{
	protocol:       domain.ProtocolOpenAI,
	decodeRequest:  openai.DecodeRequest,
	encodeRequest:  openai.EncodeRequest,
	decodeResponse: openai.DecodeResponse,
	encodeResponse: openai.EncodeResponse,
	encodeError:    openai.EncodeError,
	upstreamPath:   "/chat/completions",
}

var anthropicCodec = codec{
	protocol:       domain.ProtocolAnthropic,
	decodeRequest:  anthropic.DecodeRequest,
	encodeRequest:  anthropic.EncodeRequest,
	decodeResponse: anthropic.DecodeResponse,
	encodeResponse: anthropic.EncodeResponse,
	encodeError:    anthropic.EncodeError,
	upstreamPath:   "/messages",
}

func backendCodec(p domain.Protocol) codec {
	if p == domain.ProtocolAnthropic {
		return anthropicCodec
	}
	return openaiCodec
}

type Proxy struct {
	store  *store.Store
	client *http.Client
}

func New(s *store.Store) *Proxy {
	return &Proxy{
		store:  s,
		client: &http.Client{Timeout: 5 * time.Minute},
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
