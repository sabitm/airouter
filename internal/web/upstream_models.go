package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
)

var upstreamClient = &http.Client{Timeout: 15 * time.Second}

// providerModels fetches the selected provider's live model list and returns it
// as a datalist for the combo form's upstream_model autocomplete. Best-effort:
// any failure yields an empty datalist so combo creation still works manually.
func (h *Handler) providerModels(w http.ResponseWriter, r *http.Request) {
	listID := r.URL.Query().Get("list")
	if listID == "" {
		listID = "model-options"
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("provider_id"), 10, 64)
	if err != nil {
		render(w, r, ModelDatalist(nil, listID))
		return
	}
	provider, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		render(w, r, ModelDatalist(nil, listID))
		return
	}
	// oauth providers carry no static key; resolve (and refresh) the access token
	// onto provider.APIKey so fetchUpstreamModels can send it as a bearer. Resolve
	// does not mutate the provider, so the returned token must be assigned back.
	if provider.Method() == domain.AuthOAuth {
		tok, err := h.oauth.Resolve(r.Context(), provider, false)
		if err != nil {
			render(w, r, ModelDatalist(nil, listID))
			return
		}
		provider.APIKey = tok
	}
	models, err := fetchUpstreamModels(r.Context(), provider)
	if err != nil {
		models = nil
	}
	render(w, r, ModelDatalist(models, listID))
}

// fetchUpstreamModels queries the provider's /models endpoint. Both OpenAI and
// Anthropic return {"data":[{"id":...}]}; only the auth headers differ. The
// credential header follows the effective auth scheme (oauth always bearer),
// not the protocol, so an oauth provider speaking Anthropic still sends bearer.
func fetchUpstreamModels(ctx context.Context, p *domain.Provider) ([]string, error) {
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	switch p.Auth() {
	case domain.AuthXAPIKey:
		req.Header.Set("x-api-key", p.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	if p.Protocol == domain.ProtocolAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}
