package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"airouter/internal/domain"
)

const anthropicVersion = "2023-06-01"

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
// a request that does not look like it came from the official client.
func applyUpstreamHeaders(req *http.Request, provider *domain.Provider, clientHeaders http.Header) {
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
		applyUpstreamHeaders(req, provider, clientHeaders)
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
func (p *Proxy) forwardStream(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header) (*http.Response, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders)
		req.Header.Set("Accept", "text/event-stream")
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
