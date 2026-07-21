package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// StaticModels is the chat catalog from 9router's Antigravity registry (no image models).
var StaticModels = []string{
	"gemini-3-flash-agent",
	"gemini-3.5-flash-low",
	"gemini-3.5-flash-extra-low",
	"gemini-pro-agent",
	"gemini-3.1-pro-low",
	"claude-sonnet-4-6",
	"claude-opus-4-6-thinking",
	"gpt-oss-120b-medium",
	"gemini-3-flash",
}

const fetchModelsURL = "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"

// ListModelIDs tries the live fetchAvailableModels endpoint and falls back to StaticModels.
func ListModelIDs(ctx context.Context, accessToken, projectID string) []string {
	if accessToken == "" {
		return append([]string(nil), StaticModels...)
	}
	ids, err := fetchAvailableModels(ctx, accessToken, projectID)
	if err != nil || len(ids) == 0 {
		return append([]string(nil), StaticModels...)
	}
	return ids
}

func fetchAvailableModels(ctx context.Context, accessToken, projectID string) ([]string, error) {
	body := map[string]any{}
	if projectID != "" {
		body["project"] = projectID
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fetchModelsURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetchAvailableModels HTTP %d", resp.StatusCode)
	}

	// Response shape varies; collect string keys / model ids generously.
	var top map[string]any
	if json.Unmarshal(data, &top) != nil {
		return nil, fmt.Errorf("decode models")
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || strings.Contains(strings.ToLower(s), "image") {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	// models: [{name|id|model}] or map[id]any
	if models, ok := top["models"].([]any); ok {
		for _, m := range models {
			if mm, ok := m.(map[string]any); ok {
				for _, k := range []string{"name", "id", "model"} {
					if s, ok := mm[k].(string); ok {
						add(s)
						break
					}
				}
			} else if s, ok := m.(string); ok {
				add(s)
			}
		}
	}
	if models, ok := top["models"].(map[string]any); ok {
		for k := range models {
			add(k)
		}
	}
	if models, ok := top["model"].([]any); ok {
		for _, m := range models {
			if s, ok := m.(string); ok {
				add(s)
			}
		}
	}
	return out, nil
}
