package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var modelsClient = &http.Client{Timeout: 15 * time.Second}

// ListModelIDs fetches the provider's live model list using the Claude Code CLI
// identity headers, since the /models endpoint keys acceptance on the client
// fingerprint. accessToken is the resolved bearer token. The caller falls back
// to StaticModels on error or an empty list so dashboard autocomplete still works.
func ListModelIDs(ctx context.Context, baseURL, accessToken string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	for k, v := range IdentityHeaders() {
		req.Header.Set(k, v)
	}
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
