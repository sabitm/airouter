package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"airouter/internal/domain"
)

// checkProvider validates a base URL + API key + protocol against the live
// upstream before the provider is saved. The api_key field may be blank on an
// edit, in which case the stored key for the given id is reused.
func (h *Handler) checkProvider(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, CheckResult(false, "invalid form"))
		return
	}
	proto := domain.Protocol(r.FormValue("protocol"))
	if !proto.Valid() {
		render(w, r, CheckResult(false, "select a protocol"))
		return
	}
	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	if baseURL == "" {
		render(w, r, CheckResult(false, "enter a base URL"))
		return
	}
	apiKey := r.FormValue("api_key")
	if apiKey == "" {
		// Edit form left the key blank to keep the current one; recover it.
		if id, err := strconv.ParseInt(r.FormValue("id"), 10, 64); err == nil {
			if p, err := h.store.GetProvider(r.Context(), id); err == nil {
				apiKey = p.APIKey
			}
		}
	}
	if apiKey == "" {
		render(w, r, CheckResult(false, "enter an API key"))
		return
	}

	ok, msg := checkUpstream(r.Context(), &domain.Provider{BaseURL: baseURL, APIKey: apiKey, Protocol: proto})
	render(w, r, CheckResult(ok, msg))
}

// checkUpstream performs a GET {base_url}/models with the protocol's auth
// headers and classifies the outcome. The /models response shape is identical
// across OpenAI and Anthropic, so protocol verification is a soft signal: a
// mismatch surfaces only via a 404 or an unexpected body, not definitively.
func checkUpstream(ctx context.Context, p *domain.Provider) (bool, string) {
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "invalid base URL"
	}
	if p.Protocol == domain.ProtocolAnthropic {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return false, "could not reach URL: " + err.Error()
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("API key rejected (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return false, "not found (HTTP 404) - check base URL and protocol"
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Data == nil {
		return false, "reachable, but response shape unexpected - protocol may not match"
	}
	return true, fmt.Sprintf("OK - reachable, key accepted (%d models)", len(parsed.Data))
}
