package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/responses"
)

const anthropicVersion = "2023-06-01"

// applyCodexHeaders sets the Codex-CLI identity headers the ChatGPT backend
// requires: User-Agent, originator, session_id (the per-request CodexSessionID
// carried on the trace context), and chatgpt-account-id (from the id_token when
// the connection extracted one). session_id is also used as prompt_cache_key.
func applyCodexHeaders(req *http.Request, provider *domain.Provider, ctx context.Context) {
	req.Header.Set("User-Agent", "codex_cli_rs/"+responses.CodexCLIVersion)
	req.Header.Set("originator", "codex_cli_rs")
	if t := traceInfoFrom(ctx); t != nil && t.CodexSessionID != "" {
		req.Header.Set("session_id", t.CodexSessionID)
	}
	if provider.OAuthCreds != nil && provider.OAuthCreds.AccountID != "" {
		req.Header.Set("chatgpt-account-id", provider.OAuthCreds.AccountID)
	}
}

// newCodexSessionID returns a random id suitable for the Codex session_id header
// and prompt_cache_key. Anthropic-style UUIDs are not required here; a hex token
// is enough and avoids the format's hyphens in a header value.
func newCodexSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// prepareUpstreamRequest applies backend-specific post-encode patches to the
// upstream body that need per-request state or provider config the codec's
// encodeRequest cannot see:
//
//   - Codex: sets up the per-request session id and injects it as
//     prompt_cache_key; the id is saved on the trace context so applyCodexHeaders
//     emits it as the session_id header.
//   - Kiro: injects the provider's CodeWhisperer profile ARN into the request.
//
// Other backends return the body unchanged.
func prepareUpstreamRequest(ctx context.Context, backend codec, provider *domain.Provider, body []byte) []byte {
	switch backend.id {
	case "oai-codex":
		id := newCodexSessionID()
		if t := traceInfoFrom(ctx); t != nil {
			t.CodexSessionID = id
		}
		return responses.InjectCodexRequestKey(body, id)
	case "kiro":
		return kiro.InjectProfileArn(body, kiroProfileArn(provider))
	default:
		return body
	}
}

// kiroProfileArn returns the CodeWhisperer profile ARN configured for a Kiro
// provider, from its OAuthCreds (which carries the field for both apikey and
// oauth Kiro providers). Empty when unset.
func kiroProfileArn(provider *domain.Provider) string {
	if provider != nil && provider.OAuthCreds != nil {
		return provider.OAuthCreds.ProfileArn
	}
	return ""
}

// hopByHopOrControlled are request headers we never copy from the client: either
// the transport owns them, or we set them ourselves (auth). Dropping the client
// auth headers lets us substitute the provider's credential.
var hopByHopOrControlled = map[string]bool{
	"Host":              true,
	"Content-Length":    true,
	"Connection":        true,
	"Accept-Encoding":   true,
	"Authorization":     true,
	"X-Api-Key":         true,
	"Keep-Alive":        true,
	"Proxy-Connection":  true,
	"Transfer-Encoding": true,
}

// applyUpstreamHeaders copies the client's request headers onto the upstream
// request (under the denylist above), then sets the provider auth. Forwarding
// client headers preserves caller identity (User-Agent, x-app, anthropic-beta,
// x-stainless-*), which some providers require: an Anthropic upstream may reject
// a request that does not look like it came from the official client. ctx is the
// per-request context so codex headers (session_id) can read the trace session id.
func applyUpstreamHeaders(req *http.Request, provider *domain.Provider, clientHeaders http.Header, ctx context.Context) {
	for name, vals := range clientHeaders {
		if hopByHopOrControlled[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// Credential header depends on the auth scheme, which is independent of the
	// protocol: an Anthropic-format provider may use a bearer token.
	switch provider.Auth() {
	case domain.AuthXAPIKey:
		req.Header.Set("x-api-key", provider.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	// anthropic-version is a wire-format requirement of the Anthropic Messages
	// API, tied to protocol rather than auth. Preserve a client-sent value.
	if provider.Protocol == domain.ProtocolAnthropic && req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", anthropicVersion)
	}
	// The Codex backend additionally requires the Codex-CLI identity headers.
	// User-Agent is set after the client-header copy so it overrides any
	// forwarded client User-Agent the Codex backend would reject.
	if provider.Protocol == domain.ProtocolOpenAICodex {
		applyCodexHeaders(req, provider, ctx)
	}
	// Kiro requires the CodeWhisperer/AWS-SDK identity headers; a malformed
	// User-Agent is rejected upstream. Set after the client-header copy so they
	// override any forwarded values.
	if provider.Protocol == domain.ProtocolKiro {
		applyKiroHeaders(req, provider)
	}
	// Cline/ClinePass OAuth needs the workos: bearer prefix and Cline identity
	// headers; set after the auth-scheme switch so Authorization is rewritten.
	if provider.OAuthCreds != nil && provider.OAuthCreds.ClineAuth {
		applyClineHeaders(req, provider)
	}
}

// applyClineHeaders sets Cline identity headers and normalizes the bearer token
// with the workos: prefix the upstream requires. Auth method is already bearer
// from the scheme switch; this overwrites Authorization with the prefixed form.
func applyClineHeaders(req *http.Request, provider *domain.Provider) {
	for k, v := range oauth.ClineIdentityHeaders("", provider.APIKey) {
		req.Header.Set(k, v)
	}
}

// applyKiroHeaders sets the CodeWhisperer identity headers and, for an apikey
// provider, the tokentype marker the upstream keys host acceptance on. The
// Amz-Sdk-Invocation-Id is a fresh uuid per request. Authorization is already
// set to the bearer credential by the auth-scheme switch above.
func applyKiroHeaders(req *http.Request, provider *domain.Provider) {
	req.Header.Set("X-Amz-Target", kiro.XAmzTarget)
	req.Header.Set("User-Agent", kiro.UserAgent)
	req.Header.Set("X-Amz-User-Agent", kiro.XAmzUserAgent)
	req.Header.Set("Amz-Sdk-Request", kiro.AmzSdkRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", newUUID())
	if provider.Method() == domain.AuthAPIKey {
		req.Header.Set("tokentype", "API_KEY")
	}
}

// newUUID returns a random RFC 4122 version-4 UUID string for the
// Amz-Sdk-Invocation-Id header.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// forward sends the prepared body to the provider's upstream endpoint for the
// given backend protocol, setting the protocol-appropriate auth headers.
// clientHeaders, when non-nil (passthrough), are forwarded under the denylist.
//
// For oauth providers the access token is resolved (and proactively refreshed
// when near expiry) before the first send; on a 401/403 the token is forcibly
// refreshed and the request retried once.
func (p *Proxy) forward(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header) (int, []byte, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	send := func() (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders, ctx)
		resp, err := p.client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, nil, err
		}
		return resp.StatusCode, respBody, nil
	}

	if err := p.resolveToken(ctx, provider, false); err != nil {
		// Proactive refresh failed; proceed with the existing token (it may still
		// work, or the reactive path below will catch the 401).
		p.debugf("oauth resolve %s: %v", provider.Name, err)
	}
	status, respBody, err := send()
	if err != nil {
		return status, respBody, err
	}
	if isAuthFailure(status) && provider.Method() == domain.AuthOAuth {
		if rerr := p.resolveToken(ctx, provider, true); rerr == nil {
			return send()
		} else {
			p.debugf("oauth forced refresh %s after %d: %v", provider.Name, status, rerr)
		}
	}
	return status, respBody, nil
}

// forwardStream sends the body and returns the live response for streaming.
// The caller owns closing resp.Body. Used for SSE responses. Token resolution
// and the reactive 401/403 retry mirror forward, but must complete before the
// stream is handed back since the status is only inspected once.
func (p *Proxy) forwardStream(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header, streamAccept string) (*http.Response, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	accept := streamAccept
	if accept == "" {
		accept = "text/event-stream"
	}
	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders, ctx)
		req.Header.Set("Accept", accept)
		return p.streamClient.Do(req)
	}

	if err := p.resolveToken(ctx, provider, false); err != nil {
		p.debugf("oauth resolve %s: %v", provider.Name, err)
	}
	resp, err := send()
	if err != nil {
		return resp, err
	}
	if isAuthFailure(resp.StatusCode) && provider.Method() == domain.AuthOAuth {
		if rerr := p.resolveToken(ctx, provider, true); rerr == nil {
			resp.Body.Close()
			return send()
		} else {
			p.debugf("oauth forced refresh %s after %d: %v", provider.Name, resp.StatusCode, rerr)
		}
	}
	return resp, nil
}

// resolveToken sets provider.APIKey to the effective upstream credential. For
// oauth providers it resolves (and may refresh) the access token; for apikey
// providers Resolve returns the static key unchanged, so this is a no-op. The
// provider is the request-local hydrated copy, so the mutation is request-scoped.
// On error the best-available token is still written, so callers may proceed.
func (p *Proxy) resolveToken(ctx context.Context, provider *domain.Provider, force bool) error {
	tok, err := p.oauth.Resolve(ctx, provider, force)
	provider.APIKey = tok
	return err
}

// isAuthFailure reports whether an upstream status indicates a rejected
// credential, the trigger for a reactive OAuth token refresh.
func isAuthFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}
