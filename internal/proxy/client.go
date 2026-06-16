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

// forward sends the prepared body to the provider's upstream endpoint for the
// given backend protocol, setting the protocol-appropriate auth headers.
func (p *Proxy) forward(ctx context.Context, provider *domain.Provider, path string, body []byte) (int, []byte, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch provider.Protocol {
	case domain.ProtocolAnthropic:
		req.Header.Set("x-api-key", provider.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)
	default:
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}

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
func (p *Proxy) forwardStream(ctx context.Context, provider *domain.Provider, path string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	switch provider.Protocol {
	case domain.ProtocolAnthropic:
		req.Header.Set("x-api-key", provider.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)
	default:
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	return p.streamClient.Do(req)
}
