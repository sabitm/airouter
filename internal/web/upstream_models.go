package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/responses"
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
	if p.Protocol == domain.ProtocolOpenAICodex {
		models, _, _, err := fetchCodexModels(ctx, p, false)
		if err != nil || len(models) == 0 {
			return codexModels(), nil
		}
		return models, nil
	}
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

func fetchCodexModels(ctx context.Context, p *domain.Provider, trace bool) ([]string, int, []byte, error) {
	url := strings.TrimRight(p.BaseURL, "/") + "/models?client_version=" + responses.CodexCLIVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/"+responses.CodexCLIVersion)
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("session_id", newCheckSessionID())
	if p.OAuthCreds != nil && p.OAuthCreds.AccountID != "" {
		req.Header.Set("chatgpt-account-id", p.OAuthCreds.AccountID)
	}
	if trace {
		log.Printf("[trace] >>> GET %s", url)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< GET %s: %v", url, err)
		}
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, body, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	models, err := parseModelIDs(body)
	return models, resp.StatusCode, body, err
}

func parseModelIDs(body []byte) ([]string, error) {
	var wrapper struct {
		Data   []modelID `json:"data"`
		Models []modelID `json:"models"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil {
		if out := collectModelIDs(wrapper.Data); len(out) > 0 {
			return out, nil
		}
		if out := collectModelIDs(wrapper.Models); len(out) > 0 {
			return out, nil
		}
	}
	var arr []modelID
	if err := json.Unmarshal(body, &arr); err == nil {
		if out := collectModelIDs(arr); len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("model response shape unexpected")
}

type modelID struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func collectModelIDs(models []modelID) []string {
	out := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, m := range models {
		id := m.ID
		if id == "" {
			id = m.Slug
		}
		if id == "" {
			id = m.Name
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func codexModels() []string {
	return []string{
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex",
		"gpt-5.3-codex-spark",
		"gpt-5.3-codex-none",
		"gpt-5.3-codex-low",
		"gpt-5.3-codex-medium",
		"gpt-5.3-codex-high",
		"gpt-5.3-codex-xhigh",
		"gpt-5.3-codex-review",
	}
}
