// Package proxy implements the bidirectional inference proxy: it accepts
// requests on an OpenAI or Anthropic ingress endpoint, resolves the requested
// combo to an upstream provider, translates the payload to the provider's
// protocol when they differ, forwards it, and translates the response back.
package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/qoder"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
	"airouter/internal/store"
)

// streamEncoder renders IR stream events into an ingress-format SSE stream.
type streamEncoder interface {
	Encode(ev ir.StreamEvent, w *sse.Writer) error
	// EncodeError writes a terminal in-stream error frame for the ingress format.
	// Callers must not Encode or Close after it.
	EncodeError(w *sse.Writer, message, errType string) error
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

	// streamOnly marks a backend whose upstream returns only a stream, even for a
	// non-streaming client request: the proxy sends with the stream Accept and
	// collects the events back into a unary response. Codex and Kiro set this.
	streamOnly bool
	// streamAccept overrides the Accept header sent on a streaming upstream
	// request. Empty means text/event-stream; Kiro sets the binary EventStream
	// media type. Ignored when this codec is an ingress.
	streamAccept string
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

// responsesCodec is bidirectional: ingress when a client calls /v1/responses,
// and backend when a provider's protocol is openai-responses. Its id differs
// from oai-chat, so a Responses request to a Chat-Completions provider still
// translates; a Responses request to a Responses provider passes through.
var responsesCodec = codec{
	id:               "oai-responses",
	protocol:         domain.ProtocolOpenAIResponses,
	decodeRequest:    responses.DecodeRequest,
	encodeRequest:    responses.EncodeRequest,
	decodeResponse:   responses.DecodeResponse,
	encodeResponse:   responses.EncodeResponse,
	encodeError:      responses.EncodeError,
	upstreamPath:     "/responses",
	decodeStream:     responses.DecodeStream,
	newStreamEncoder: func(model string) streamEncoder { return responses.NewStreamEncoder(model) },
}

// codexCodec is the ChatGPT-backend Codex provider: same Responses wire format
// as responsesCodec, but the request encoder enforces the Codex envelope
// (store=false, effort suffix, default instructions) and the proxy injects the
// Codex-CLI identity headers. Its id differs from oai-responses so a Responses
// request to a Codex provider still translates through the Codex encoder rather
// than passing through.
var codexCodec = codec{
	id:               "oai-codex",
	protocol:         domain.ProtocolOpenAICodex,
	decodeRequest:    responses.DecodeRequest,
	encodeRequest:    responses.EncodeCodexRequest,
	decodeResponse:   responses.DecodeResponse,
	encodeResponse:   responses.EncodeResponse,
	encodeError:      responses.EncodeError,
	upstreamPath:     "/responses",
	decodeStream:     responses.DecodeStream,
	newStreamEncoder: func(model string) streamEncoder { return responses.NewStreamEncoder(model) },
	streamOnly:       true,
}

// kiroCodec is the AWS CodeWhisperer-backed Kiro backend. It is backend-only
// (no ingress route and no decodeRequest): every request translates through the
// IR. The upstream returns a binary AWS EventStream, so it is stream-only and
// its streaming Accept differs from the SSE default. There is no unary response
// decoder; unary client requests are collected from the stream.
var kiroCodec = codec{
	id:            "kiro",
	protocol:      domain.ProtocolKiro,
	encodeRequest: kiro.EncodeRequest,
	upstreamPath:  kiro.UpstreamPath,
	decodeStream:  kiro.DecodeStream,
	streamOnly:    true,
	streamAccept:  kiro.EventStreamAccept,
}

// qoderCodec is the Qoder backend: COSY-signed WAF-encoded chat, SSE-only.
// Backend-only; every request translates through the IR. Unary clients collect
// from the stream. Device tokens do not refresh.
var qoderCodec = codec{
	id:            "qoder",
	protocol:      domain.ProtocolQoder,
	encodeRequest: qoder.EncodeRequest,
	upstreamPath:  qoder.UpstreamPath,
	decodeStream:  qoder.DecodeStream,
	streamOnly:    true,
}

// antigravityCodec is Google Antigravity / Cloud Code: Gemini-in-envelope chat,
// SSE-only. Backend-only; every request translates through the IR.
var antigravityCodec = codec{
	id:            "antigravity",
	protocol:      domain.ProtocolAntigravity,
	encodeRequest: antigravity.EncodeRequest,
	upstreamPath:  antigravity.UpstreamPathStream,
	decodeStream:  antigravity.DecodeStream,
	streamOnly:    true,
}

// cursorCodec is the Cursor IDE backend: Connect-RPC protobuf chat
// (ChatService StreamUnifiedChatWithTools), stream-only. Backend-only; every
// request translates through the IR. Auth is a pasted IDE token + machine id;
// tokens are not refreshable.
var cursorCodec = codec{
	id:            "cursor",
	protocol:      domain.ProtocolCursor,
	encodeRequest: cursor.EncodeRequest,
	upstreamPath:  cursor.UpstreamPath,
	decodeStream:  cursor.DecodeStream,
	streamOnly:    true,
	streamAccept:  cursor.StreamAccept,
}

// claudeCodeCodec is the Claude Code CLI backend: Anthropic Messages wire
// format spoken with the Claude Code client identity, claude.ai OAuth, and an
// OAuth-only tool cloak/decoy transform. It reuses the anthropic codec's
// encoder for the base body and its stream encoder / error envelope; the cloak
// is applied in prepareUpstreamRequest (where the resolved token and per-request
// session id are visible) and decloaking in the response/stream wrappers. Its id
// differs from anth-msg so Anthropic ingress never passes through. Anthropic
// Messages supports unary and SSE, so it is not stream-only.
var claudeCodeCodec = codec{
	id:               "claude-code",
	protocol:         domain.ProtocolClaudeCode,
	decodeRequest:    anthropic.DecodeRequest,
	encodeRequest:    anthropic.EncodeRequestClaudeCode,
	decodeResponse:   claudecode.DecodeResponse,
	encodeResponse:   anthropic.EncodeResponse,
	encodeError:      anthropic.EncodeError,
	upstreamPath:     claudecode.UpstreamPath,
	decodeStream:     claudecode.DecodeStream,
	newStreamEncoder: func(model string) streamEncoder { return anthropic.NewStreamEncoder(model) },
}

func backendCodec(p domain.Protocol) codec {
	switch p {
	case domain.ProtocolAnthropic:
		return anthropicCodec
	case domain.ProtocolOpenAIResponses:
		return responsesCodec
	case domain.ProtocolOpenAICodex:
		return codexCodec
	case domain.ProtocolKiro:
		return kiroCodec
	case domain.ProtocolQoder:
		return qoderCodec
	case domain.ProtocolAntigravity:
		return antigravityCodec
	case domain.ProtocolCursor:
		return cursorCodec
	case domain.ProtocolClaudeCode:
		return claudeCodeCodec
	default:
		return openaiCodec
	}
}

type Proxy struct {
	store *store.Store
	// oauth resolves the effective upstream bearer token for oauth providers,
	// refreshing access tokens as needed. apikey providers pass through unchanged.
	oauth *oauth.Service
	// client bounds unary requests; streamClient has no total timeout so long
	// SSE streams are governed by the request context instead.
	client       *http.Client
	streamClient *http.Client
	logger       *slog.Logger

	// rr holds per-combo round-robin counters, keyed by combo id. In-memory only:
	// the rotation resets on restart, which is acceptable for load spreading.
	rrMu sync.Mutex
	rr   map[int64]uint64

	// bo holds per-provider failover backoff state, keyed by provider id. A
	// provider that fails over (before committing any bytes) is deferred behind
	// healthy providers for an exponentially growing number of subsequent requests,
	// so a persistently failing target (e.g. out of balance) is not retried first
	// on every request. Keyed by provider, not combo target, because a provider's
	// health is shared across every combo it appears in. In-memory only: it resets
	// on restart.
	boMu sync.Mutex
	bo   map[int64]*backoffState
}

// backoffState tracks a provider's consecutive pre-commit failures and the number
// of subsequent requests for which it should still be deferred behind healthy
// targets. skips is consumed one credit per request that resolves the combo.
type backoffState struct {
	failures int
	skips    int
}

const (
	// A provider's first failure defers it for backoffBaseSkips requests, doubling
	// per consecutive failure up to backoffMaxSkips.
	backoffBaseSkips = 2
	backoffMaxSkips  = 256
	// backoffShiftCap bounds the exponent so base<<shift cannot overflow int; the
	// result is clamped to backoffMaxSkips well before this anyway.
	backoffShiftCap = 30
)

// New builds a Proxy. logger may be nil (falls back to slog.Default). Prefer a
// component=proxy logger from the server constructor so attrs stay consistent.
// HAR capture is request-pinned via TraceInfo.HAR set by server middleware.
func New(s *store.Store, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{
		store:        s,
		oauth:        oauth.New(s),
		client:       &http.Client{Timeout: 5 * time.Minute},
		streamClient: &http.Client{},
		logger:       logger,
		rr:           map[int64]uint64{},
		bo:           map[int64]*backoffState{},
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

// providerBackedOff reports whether a provider should be deferred behind healthy
// targets for this request, consuming one skip credit when it is. Consuming here
// means each call represents one request's worth of deferral, so a provider
// penalized for N skips is deferred for exactly the next N requests that consult
// it, after which it becomes eligible again (and is re-probed).
func (p *Proxy) providerBackedOff(providerID int64) bool {
	p.boMu.Lock()
	defer p.boMu.Unlock()
	st, ok := p.bo[providerID]
	if !ok || st.skips <= 0 {
		return false
	}
	st.skips--
	return true
}

// penalizeProvider records a failover for a provider, setting the number of
// subsequent requests it is deferred for exponentially with the consecutive
// failure count (backoffBaseSkips * 2^(n-1), clamped to backoffMaxSkips). Called
// when an attempt fails before committing bytes.
func (p *Proxy) penalizeProvider(providerID int64) {
	p.boMu.Lock()
	defer p.boMu.Unlock()
	st := p.bo[providerID]
	if st == nil {
		st = &backoffState{}
		p.bo[providerID] = st
	}
	st.failures++
	shift := st.failures - 1
	if shift > backoffShiftCap {
		shift = backoffShiftCap
	}
	n := backoffBaseSkips << uint(shift)
	if n > backoffMaxSkips || n <= 0 {
		n = backoffMaxSkips
	}
	st.skips = n
}

// clearBackoff resets a provider's penalty state after a committed success, so a
// recovered provider is immediately eligible again.
func (p *Proxy) clearBackoff(providerID int64) {
	p.boMu.Lock()
	delete(p.bo, providerID)
	p.boMu.Unlock()
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
