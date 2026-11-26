package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/opencode"
)

func (p *Proxy) doUpstreamRequest(req *http.Request, provider *domain.Provider, body []byte, stream bool) (*http.Response, error) {
	client := p.upstreamClient(provider, req.URL.String(), body, stream)
	if provider != nil && provider.OAuthCreds != nil && provider.OAuthCreds.OpencodeConsoleAuth {
		client = opencode.ClientNoRedirect(client)
	}
	if provider != nil && (provider.Protocol == domain.ProtocolGrokBuild ||
		provider.OAuthCreds != nil && provider.OAuthCreds.GrokBuildAuth) {
		client = grokbuild.ClientNoRedirect(client)
	}
	// Opaque Codex replay must not follow a redirect onto another host. Clone
	// the client so the shared proxy client keeps its redirect policy.
	if req != nil && codexReplayGuardFrom(req.Context()) != nil {
		client = codexClientNoRedirect(client)
	}
	if p.logger == nil || !p.logger.Enabled(req.Context(), observability.LevelTrace) ||
		provider == nil || provider.Protocol != domain.ProtocolOpencode ||
		!opencode.IsOfficialFreeTierURL(req.URL.String()) ||
		!opencode.NeedsFreeTier(effectiveProviderBaseURL(provider), reqModelFromBody(body)) {
		return client.Do(req)
	}

	started := time.Now()
	var mu sync.Mutex
	var acquisitions int
	var lastConn httptrace.GotConnInfo
	var firstByte time.Duration
	var gotFirstByte bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			acquisitions++
			// Keep only metadata. The connection can expose addresses and must not be logged.
			lastConn = httptrace.GotConnInfo{Reused: info.Reused, WasIdle: info.WasIdle, IdleTime: info.IdleTime}
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			if !gotFirstByte {
				firstByte = time.Since(started)
				gotFirstByte = true
			}
			mu.Unlock()
		},
	}
	traced := req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(traced)
	elapsed := time.Since(started)
	mu.Lock()
	attrs := []any{
		"event", "opencode_upstream_send",
		"provider_id", provider.ID,
		"stream", stream,
		"connection_acquisitions", acquisitions,
		"send_ms", elapsed.Milliseconds(),
		"error_category", opencodeSendErrorCategory(err),
	}
	if acquisitions > 0 {
		attrs = append(attrs,
			"connection_reused", lastConn.Reused,
			"connection_was_idle", lastConn.WasIdle,
			"connection_idle_ms", lastConn.IdleTime.Milliseconds(),
		)
	} else {
		attrs = append(attrs, "connection_reused", "unknown", "connection_was_idle", "unknown", "connection_idle_ms", "unknown")
	}
	if gotFirstByte {
		attrs = append(attrs, "first_byte_ms", firstByte.Milliseconds())
	} else {
		attrs = append(attrs, "first_byte_ms", "unknown")
	}
	mu.Unlock()

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	attrs = append(attrs, "status", status)
	if resp != nil && err == nil {
		attrs = append(attrs, "headers_ms", elapsed.Milliseconds())
	} else {
		attrs = append(attrs, "headers_ms", "unknown")
	}
	attrs = append(attrs, p.opencodeSessionDiagnosticAttrs(req)...)
	log := observability.Logger(req.Context(), p.logger)
	if observability.RequestID(req.Context()) == "" {
		if info := traceInfoFrom(req.Context()); info != nil && info.RequestID != "" {
			log = log.With("request_id", info.RequestID)
		}
	}
	log.Log(req.Context(), observability.LevelTrace, "opencode_upstream_send", attrs...)
	return resp, err
}

func (p *Proxy) opencodeSessionDiagnosticAttrs(req *http.Request) []any {
	attrs := []any{
		"upstream_session", p.opencodeSessionPseudonym("upstream", req.Header.Get("x-opencode-session")),
	}
	st := opencodeStateFrom(req.Context())
	if st == nil {
		return append(attrs, "client_session", "unknown", "session_resolution", "unknown", "session_cache", "unknown", "session_cache_age_ms", "unknown")
	}
	attrs = append(attrs, "client_session", p.opencodeSessionPseudonym("client", st.identity.Session))
	if st.session == "" || st.session != req.Header.Get("x-opencode-session") || st.sessionResolution == "" {
		return append(attrs, "session_resolution", "unknown", "session_cache", "unknown", "session_cache_age_ms", "unknown")
	}
	attrs = append(attrs, "session_resolution", st.sessionResolution)
	if !st.sessionLookup.Mapped {
		return append(attrs, "session_cache", "not_applicable", "session_cache_age_ms", "unknown")
	}
	cache := "miss"
	if st.sessionLookup.Hit {
		cache = "hit"
	}
	age := time.Since(st.sessionLookup.CreatedAt)
	if age < 0 {
		age = 0
	}
	return append(attrs, "session_cache", cache, "session_cache_age_ms", age.Milliseconds())
}

func (p *Proxy) opencodeSessionPseudonym(namespace, session string) string {
	// The existing random per-Proxy nonce keeps pseudonyms unlinkable across restarts.
	// Without that key, do not fall back to an unkeyed hash of client-controlled IDs.
	if p.opencodeNonce == "" || session == "" {
		return "unknown"
	}
	h := hmac.New(sha256.New, []byte(p.opencodeNonce))
	h.Write([]byte("opencode-diagnostics\x00" + namespace + "\x00" + session))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func opencodeSendErrorCategory(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return "timeout"
		}
		return "transport"
	}
}
