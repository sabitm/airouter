package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
)

// Cloud Code Assist endpoints for Antigravity project bootstrap. Vars (not
// consts) so tests can point them at an httptest server.
var (
	agLoadCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	agOnboardUserURL    = "https://cloudcode-pa.googleapis.com/v1internal:onboardUser"
	agUserInfoURL       = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
)

const (
	agOnboardTimeout   = 60 * time.Second
	agOnboardPollEvery = 5 * time.Second
)

// finalizeAntigravity runs after a successful token exchange for AntigravityAuth
// connections: userinfo + loadCodeAssist + onboardUser. Fails closed without a
// ProjectID so the provider is not saved half-connected.
func finalizeAntigravity(ctx context.Context, c *domain.OAuthCreds) error {
	if c == nil || !c.AntigravityAuth {
		return nil
	}
	if c.AccessToken == "" {
		return fmt.Errorf("oauth: antigravity finalize: empty access token")
	}
	if err := fetchAntigravityUserInfo(ctx, c); err != nil {
		// Email is nice-to-have; project is required. Continue if userinfo fails.
		// Still try project bootstrap.
		_ = err
	}
	projectID, tierID, err := loadCodeAssist(ctx, c.AccessToken)
	if err != nil {
		return fmt.Errorf("oauth: antigravity loadCodeAssist: %w", err)
	}
	// Fresh Google accounts return 200 with allowedTiers but no project.
	// onboardUser provisions cloudaicompanionProject; fail only after that.
	finalID, err := completeOnboarding(ctx, c.AccessToken, projectID, tierID)
	if err != nil {
		return fmt.Errorf("oauth: antigravity onboard: %w", err)
	}
	if finalID != "" {
		projectID = finalID
	}
	if projectID == "" {
		return fmt.Errorf("oauth: antigravity: no project id after onboarding (tier %q); reconnect", tierID)
	}
	c.ProjectID = projectID
	return nil
}

func fetchAntigravityUserInfo(ctx context.Context, c *domain.OAuthCreds) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agUserInfoURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("userinfo HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}
	var u struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(body, &u) != nil {
		return fmt.Errorf("userinfo decode")
	}
	if u.Email != "" {
		c.Email = u.Email
	}
	if u.Name != "" && c.DisplayName == "" {
		c.DisplayName = u.Name
	}
	return nil
}

func loadCodeAssist(ctx context.Context, accessToken string) (projectID, tierID string, err error) {
	meta := clientMetadata()
	payload, _ := json.Marshal(map[string]any{"metadata": meta})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agLoadCodeAssistURL, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	setCodeAssistHeaders(req, accessToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil {
		return "", "", fmt.Errorf("decode")
	}
	projectID = extractProjectID(data["cloudaicompanionProject"])
	tierID = "legacy-tier"
	if current, _ := data["currentTierId"].(string); strings.TrimSpace(current) != "" {
		tierID = strings.TrimSpace(current)
	} else if tiers, ok := data["allowedTiers"].([]any); ok {
		for _, t := range tiers {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if def, _ := tm["isDefault"].(bool); def {
				if id, _ := tm["id"].(string); strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}
	return projectID, tierID, nil
}

func completeOnboarding(ctx context.Context, accessToken, projectID, tierID string) (string, error) {
	deadline := time.Now().Add(agOnboardTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		done, finalID, err := onboardUserOnce(ctx, accessToken, tierID)
		if err != nil {
			return "", err
		}
		if done {
			if finalID != "" {
				return finalID, nil
			}
			return projectID, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("onboarding timeout")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(agOnboardPollEvery):
		}
	}
}

func onboardUserOnce(ctx context.Context, accessToken, tierID string) (done bool, projectID string, err error) {
	payload, _ := json.Marshal(map[string]any{
		"tierId":   tierID,
		"metadata": clientMetadata(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agOnboardUserURL, bytes.NewReader(payload))
	if err != nil {
		return false, "", err
	}
	setCodeAssistHeaders(req, accessToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return false, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil {
		return false, "", fmt.Errorf("decode")
	}
	if d, _ := data["done"].(bool); d {
		if respObj, ok := data["response"].(map[string]any); ok {
			return true, extractProjectID(respObj["cloudaicompanionProject"]), nil
		}
		return true, "", nil
	}
	return false, "", nil
}

func setCodeAssistHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Google fingerprints X-Goog-Api-Client/Client-Metadata on loadCodeAssist
	// and onboardUser and silently omits cloudaicompanionProject. The real
	// Antigravity IDE does not send those headers; metadata stays in the body.
	req.Header.Set("User-Agent", antigravity.UserAgent)
}

func clientMetadata() map[string]int {
	// Numeric enums matching Antigravity binary ClientMetadata (ideType=9, pluginType=2).
	// Platform: 1=darwin, 2=win, 3=linux — fixed linux is fine for server-side bootstrap.
	return map[string]int{"ideType": 9, "platform": 3, "pluginType": 2}
}

func extractProjectID(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		if id, ok := t["id"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func truncateBody(b []byte) string {
	const n = 200
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// EnsureAntigravityProject runs finalize when ProjectID is still empty after
// tokens are present (manual paste path). No-op when already set or not AG.
func EnsureAntigravityProject(ctx context.Context, c *domain.OAuthCreds) error {
	if c == nil || !c.AntigravityAuth {
		return nil
	}
	if strings.TrimSpace(c.ProjectID) != "" {
		return nil
	}
	return finalizeAntigravity(ctx, c)
}
