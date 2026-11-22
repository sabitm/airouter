package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
)

var modelsClient = &http.Client{Timeout: 15 * time.Second}

// ListModelIDs fetches the provider's live model list using the Claude Code CLI
// identity headers, since the /models endpoint keys acceptance on the client
// fingerprint. The provider carries the resolved bearer token and the grant
// snapshot used to classify the oauth beta. The caller falls back to
// StaticModels on error or an empty list so dashboard autocomplete still works.
func ListModelIDs(ctx context.Context, provider *domain.Provider) ([]string, error) {
	if provider == nil {
		return nil, fmt.Errorf("claude-code: provider is nil")
	}
	url := strings.TrimRight(provider.BaseURL, "/") + "/models?limit=1000"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	for k, v := range StainlessRuntimeHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("anthropic-version", AnthropicVersion)
	req.Header.Set("User-Agent", DefaultProfile().UserAgent())
	req.Header.Set("X-App", "cli")
	// A model-list probe has no thinking, tools, or cache policy. The oauth
	// beta follows the paired grant on this provider. Acceptance here is not
	// Messages proof.
	req.Header.Set("anthropic-beta", BetaHeaders(BetaRequest{OAuth: OAuthBeta(provider)}))
	resp, err := modelsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
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
