package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"airouter/internal/domain"
)

// Kiro has no /models endpoint and speaks CodeWhisperer JSON-1.0, so both the
// provider Check and the combo model autocomplete need Kiro-specific handling.

// kiroModels is the static Kiro model catalog used for combo autocomplete. Live
// discovery (ListAvailableModels) is out of scope; these are the base ids from
// KIRO.md 10.1. Synthetic -thinking/-agentic suffixes are not implemented, so
// they are not listed.
func kiroModels() []string {
	return []string{
		"claude-sonnet-5",
		"claude-sonnet-4.5",
		"claude-haiku-4.5",
		"deepseek-3.2",
		"qwen3-coder-next",
		"glm-5",
		"MiniMax-M2.5",
		"auto",
	}
}

// checkKiroUpstream validates OAuth Kiro credentials with ListAvailableProfiles,
// the lightweight CodeWhisperer call that confirms a bearer token is accepted
// without sending a chat request. Kiro API keys cannot use that admin API, so an
// API-key provider reports stored-only and is validated on real traffic.
func checkKiroUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	if p.Method() == domain.AuthAPIKey {
		return true, "OK - API key stored; validated on real traffic (no lightweight check endpoint accepts API keys)"
	}

	region := "us-east-1"
	if p.OAuthCreds != nil && p.OAuthCreds.Region != "" {
		region = p.OAuthCreds.Region
	}
	url := fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{"maxResults":10}`)))
	if err != nil {
		return false, "invalid region"
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("x-amz-target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	if trace {
		log.Printf("[trace] >>> POST %s (ListAvailableProfiles)", url)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< POST %s: %v", url, err)
		}
		return false, "could not reach Kiro: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body, traceMaxBody))
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("credential rejected (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d: %s", resp.StatusCode, upstreamErrorText(body))
	}

	var parsed struct {
		Profiles []struct {
			Arn        string `json:"arn"`
			ProfileArn string `json:"profileArn"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return true, "OK - credential accepted"
	}
	if arn := firstProfileArn(parsed.Profiles); arn != "" {
		return true, "OK - credential accepted, profile: " + arn
	}
	return true, fmt.Sprintf("OK - credential accepted (%d profiles)", len(parsed.Profiles))
}

func firstProfileArn(profiles []struct {
	Arn        string `json:"arn"`
	ProfileArn string `json:"profileArn"`
}) string {
	for _, p := range profiles {
		if p.ProfileArn != "" {
			return p.ProfileArn
		}
		if p.Arn != "" {
			return p.Arn
		}
	}
	return ""
}
