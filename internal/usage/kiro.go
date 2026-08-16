package usage

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/kiro"
)

func (s *Service) fetchKiro(ctx context.Context, p *domain.Provider) (*Report, error) {
	token, err := s.resolveToken(ctx, p, false)
	if err != nil {
		return nil, err
	}
	headers := kiroUsageHeaders(p)
	profileArn := kiroProfileArn(p)
	cwHost := KiroUsageBase
	if host := kiroHostFromBase(p.BaseURL); host != "" {
		cwHost = host
	}

	type attempt struct {
		name string
		run  func(tok string) (httpResult, error)
	}
	attempts := []attempt{
		{
			name: "codewhisperer-get",
			run: func(tok string) (httpResult, error) {
				q := url.Values{
					"isEmailRequired": {"true"},
					"origin":          {"AI_EDITOR"},
					"resourceType":    {"AGENTIC_REQUEST"},
				}
				return s.doJSON(ctx, http.MethodGet, cwHost+"/getUsageLimits?"+q.Encode(), tok, headers, nil)
			},
		},
		{
			name: "codewhisperer-post",
			run: func(tok string) (httpResult, error) {
				h := cloneHeaders(headers)
				h["Content-Type"] = "application/x-amz-json-1.0"
				h["x-amz-target"] = "AmazonCodeWhispererService.GetUsageLimits"
				body := map[string]any{
					"origin":       "AI_EDITOR",
					"resourceType": "AGENTIC_REQUEST",
				}
				if profileArn != "" {
					body["profileArn"] = profileArn
				}
				return s.doJSON(ctx, http.MethodPost, cwHost, tok, h, body)
			},
		},
		{
			name: "q-get",
			run: func(tok string) (httpResult, error) {
				q := url.Values{
					"origin":       {"AI_EDITOR"},
					"resourceType": {"AGENTIC_REQUEST"},
				}
				if profileArn != "" {
					q.Set("profileArn", profileArn)
				}
				return s.doJSON(ctx, http.MethodGet, KiroQUsageBase+"/getUsageLimits?"+q.Encode(), tok, headers, nil)
			},
		},
	}

	sawAuth := false
	var lastStatus int
	triedRefresh := false
	for i := 0; i < len(attempts); i++ {
		res, rerr := attempts[i].run(token)
		if rerr != nil {
			lastStatus = 0
			continue
		}
		if res.Status == http.StatusOK {
			rep, perr := parseKiroQuota(res.Body)
			if perr != nil {
				return soft("Kiro", "Kiro connected. Usage response was not JSON."), nil
			}
			return rep, nil
		}
		if res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden {
			sawAuth = true
			// Force-refresh once on the first auth failure, then retry this attempt.
			if !triedRefresh && p.Method() == domain.AuthOAuth {
				triedRefresh = true
				if refreshed, rerr := s.resolveToken(ctx, p, true); rerr == nil && refreshed != "" && refreshed != token {
					token = refreshed
					i--
					continue
				}
			}
		}
		lastStatus = res.Status
	}
	if sawAuth {
		return soft("Kiro", "Kiro connected. Quota API unavailable (auth rejected). Chat may still work."), nil
	}
	if lastStatus > 0 {
		return softf("Kiro", "Kiro connected. Quota API unavailable (%d). Chat may still work.", lastStatus), nil
	}
	return soft("Kiro", "Kiro connected. Unable to fetch usage. Chat may still work."), nil
}

func kiroUsageHeaders(p *domain.Provider) map[string]string {
	h := map[string]string{
		"Accept":           "application/json",
		"user-agent":       kiro.UserAgent,
		"x-amz-user-agent": kiro.XAmzUserAgent,
	}
	if p.Method() == domain.AuthAPIKey {
		h["tokentype"] = "API_KEY"
	} else if p.OAuthCreds != nil && strings.EqualFold(p.OAuthCreds.KiroAuth, "external_idp") {
		h["TokenType"] = "EXTERNAL_IDP"
	}
	return h
}

func kiroProfileArn(p *domain.Provider) string {
	if p.OAuthCreds == nil {
		return ""
	}
	return strings.TrimSpace(p.OAuthCreds.ProfileArn)
}

func kiroHostFromBase(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/")
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func parseKiroQuota(body []byte) (*Report, error) {
	data, err := decodeMap(body)
	if err != nil {
		return nil, err
	}
	resetAt := parseResetTime(lookupFirst(data, "nextDateReset", "resetDate"))
	var quotas []Quota
	list, _ := data["usageBreakdownList"].([]any)
	for _, item := range list {
		b := asMap(item)
		if b == nil {
			continue
		}
		resource := strings.ToLower(asString(b["resourceType"]))
		if resource == "" {
			resource = "unknown"
		}
		used := toFinite(b["currentUsageWithPrecision"], 0)
		total := toFinite(b["usageLimitWithPrecision"], 0)
		quotas = append(quotas, Quota{
			Name:      resource,
			Used:      used,
			Total:     total,
			Remaining: total - used,
			ResetAt:   resetAt,
		})
		if trial := asMap(b["freeTrialInfo"]); trial != nil {
			freeUsed := toFinite(trial["currentUsageWithPrecision"], 0)
			freeTotal := toFinite(trial["usageLimitWithPrecision"], 0)
			trialReset := parseResetTime(trial["freeTrialExpiry"])
			if trialReset == nil {
				trialReset = resetAt
			}
			quotas = append(quotas, Quota{
				Name:      fmt.Sprintf("%s (free trial)", resource),
				Used:      freeUsed,
				Total:     freeTotal,
				Remaining: freeTotal - freeUsed,
				ResetAt:   trialReset,
			})
		}
	}
	plan := "Kiro"
	if sub := asMap(data["subscriptionInfo"]); sub != nil {
		if title := asString(sub["subscriptionTitle"]); title != "" {
			plan = title
		}
	}
	return &Report{Plan: plan, Quotas: quotas, FetchedAt: time.Now()}, nil
}
