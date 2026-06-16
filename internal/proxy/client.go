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
	switch provider.Protocol {
	case domain.ProtocolAnthropic:
		req.Header.Set("x-api-key", provider.APIKey)
		// Preserve the client's anthropic-version if it sent one.
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", anthropicVersion)
		}
	default:
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
}

// forward sends the prepared body to the provider's upstream endpoint for the
// given backend protocol, setting the protocol-appropriate auth headers.
// clientHeaders, when non-nil (passthrough), are forwarded under the denylist.
func (p *Proxy) forward(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header) (int, []byte, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
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

// forwardStream sends the body and returns the live response for streaming.
// The caller owns closing resp.Body. Used for SSE responses.
func (p *Proxy) forwardStream(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header) (*http.Response, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyUpstreamHeaders(req, provider, clientHeaders)
	req.Header.Set("Accept", "text/event-stream")
	return p.streamClient.Do(req)
}
