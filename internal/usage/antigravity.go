package usage

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
)

func (s *Service) fetchAntigravity(ctx context.Context, p *domain.Provider) (*Report, error) {
	projectID := ""
	if p.OAuthCreds != nil {
		projectID = strings.TrimSpace(p.OAuthCreds.ProjectID)
	}
	// Chat fails closed without ProjectID; the quota endpoint is the same.
	if projectID == "" {
		return soft("Antigravity", "Antigravity project ID is missing. Reconnect OAuth."), nil
	}

	headers := map[string]string{
		"Content-Type":     "application/json",
		"User-Agent":       antigravity.UserAgent,
		"X-Client-Name":    "antigravity",
		"X-Client-Version": antigravity.IDEVersion,
	}
	plan := "Antigravity"
	if loaded, ok := s.loadAntigravityPlan(ctx, p); ok && loaded != "" {
		plan = loaded
	}

	res, _, err := s.doAuthed(ctx, p, http.MethodPost, AntigravityModelsURL, headers, map[string]any{"project": projectID})
	if err != nil {
		if isLocalErr(err) {
			return nil, err
		}
		return softf(plan, "Antigravity connected. Unable to fetch usage: %s", err.Error()), nil
	}
	if res.Status == http.StatusForbidden {
		return soft(plan, "Antigravity quota API access forbidden. Chat may still work."), nil
	}
	if res.Status == http.StatusUnauthorized {
		return soft(plan, "Antigravity quota API authentication expired. Chat may still work."), nil
	}
	if res.Status != http.StatusOK {
		return softf(plan, "Antigravity connected. Quota API unavailable (%d). Chat may still work.", res.Status), nil
	}
	data, err := decodeMap(res.Body)
	if err != nil {
		return soft(plan, "Antigravity connected. Usage response was not JSON."), nil
	}
	models := asMap(data["models"])
	if models == nil {
		if inner := asMap(data["data"]); inner != nil {
			models = asMap(inner["models"])
		}
	}
	var quotas []Quota
	for key, raw := range models {
		info := asMap(raw)
		if info == nil {
			continue
		}
		if isTruthy(info["isInternal"]) {
			continue
		}
		qi := asMap(info["quotaInfo"])
		if qi == nil {
			continue
		}
		frac := toFinite(qi["remainingFraction"], 0)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		remainingPct := frac * 100
		name := asString(info["displayName"])
		if name == "" {
			name = key
		}
		quotas = append(quotas, Quota{
			Name:         name,
			RemainingPct: pctPtr(remainingPct),
			ResetAt:      parseResetTime(qi["resetTime"]),
		})
	}
	sort.Slice(quotas, func(i, j int) bool { return quotas[i].Name < quotas[j].Name })
	return &Report{Plan: plan, Quotas: quotas, FetchedAt: time.Now()}, nil
}

func (s *Service) loadAntigravityPlan(ctx context.Context, p *domain.Provider) (string, bool) {
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   antigravity.UserAgent,
	}
	// Numeric enums matching oauth.clientMetadata / Antigravity IDE ClientMetadata.
	body := map[string]any{"metadata": map[string]int{"ideType": 9, "platform": 3, "pluginType": 2}}
	res, _, err := s.doAuthed(ctx, p, http.MethodPost, AntigravityLoadURL, headers, body)
	if err != nil || res.Status != http.StatusOK {
		return "", false
	}
	data, err := decodeMap(res.Body)
	if err != nil {
		return "", false
	}
	if current := asMap(data["currentTier"]); current != nil {
		if name := asString(current["name"]); name != "" {
			return name, true
		}
	}
	if name := asString(data["currentTierId"]); name != "" {
		return name, true
	}
	return "", false
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1"
	default:
		n, ok := asFloat(v)
		return ok && n != 0
	}
}
