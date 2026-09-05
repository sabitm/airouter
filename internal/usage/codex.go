package usage

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"airouter/internal/domain"
	"airouter/internal/proxy/responses"
)

func codexResetHeaders(p *domain.Provider) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"originator":   "codex_cli_rs",
		"User-Agent":   "codex_cli_rs/" + responses.CodexCLIVersion,
	}
	if p != nil && p.OAuthCreds != nil {
		if id := strings.TrimSpace(p.OAuthCreds.AccountID); id != "" {
			headers["chatgpt-account-id"] = id
		}
	}
	return headers
}

func (s *Service) storeReport(id int64, gen uint64, report *Report) {
	if id == 0 || report == nil {
		return
	}
	s.mu.Lock()
	if s.cacheGen[id] == gen {
		s.cache[id] = cacheEntry{report: report, expiresAt: s.now().Add(cacheTTL)}
	}
	s.mu.Unlock()
}

func (s *Service) ConsumeCodexResetCredit(ctx context.Context, p *domain.Provider) (*CodexResetResult, *Report, error) {
	if p == nil || p.Protocol != domain.ProtocolOpenAICodex {
		return nil, nil, ErrUnsupported
	}

	headers := codexResetHeaders(p)
	// Mint once before doAuthed: the 401 retry reuses this body. A new UUID per
	// attempt would risk double spend; already_redeemed on the same id is safe.
	redeemID := uuid.NewString()
	res, _, err := s.doAuthed(ctx, p, http.MethodPost, CodexResetConsumeURL, headers, map[string]string{
		"redeem_request_id": redeemID,
	})
	if err != nil {
		if isLocalErr(err) {
			return nil, nil, err
		}
		return &CodexResetResult{Message: "Codex connected. Unable to consume reset credit."}, nil, nil
	}
	if res.Status != http.StatusOK {
		return &CodexResetResult{Message: fmt.Sprintf("Codex connected. Reset credit API unavailable (%d).", res.Status)}, nil, nil
	}

	data, err := decodeMap(res.Body)
	if err != nil {
		return &CodexResetResult{Message: "Unexpected reset credit response."}, nil, nil
	}
	code := CodexResetCode(asString(data["code"]))
	windows := int(toFinite(lookupFirst(data, "windows_reset", "windowsReset"), 0))
	success := code == CodexResetReset || code == CodexResetAlreadyRedeemed ||
		(windows > 0 && code != CodexResetNothingToReset && code != CodexResetNoCredit)

	result := &CodexResetResult{Code: code, WindowsReset: windows}
	switch {
	case success:
		result.Message = "Usage reset."
	case code == CodexResetNothingToReset:
		result.Message = "Usage does not need a reset right now."
	case code == CodexResetNoCredit:
		result.Message = "No reset credits available."
	default:
		result.Message = "Unexpected reset credit response."
	}

	s.logger.Debug("usage_codex_reset",
		"event", "usage_codex_reset",
		"provider_id", p.ID,
		"status", res.Status,
		"code", string(code),
		"windows_reset", windows,
	)

	s.dropCache(p.ID)
	s.mu.Lock()
	gen := s.cacheGen[p.ID]
	s.mu.Unlock()
	rep, ferr := s.fetchLive(ctx, p)
	if ferr == nil && rep != nil {
		rep.Message = result.Message
		s.storeReport(p.ID, gen, rep)
		return result, rep, nil
	}
	return result, nil, nil
}

func (s *Service) fetchCodex(ctx context.Context, p *domain.Provider) (*Report, error) {
	res, _, err := s.doAuthed(ctx, p, http.MethodGet, CodexUsageURL, nil, nil)
	if err != nil {
		if isLocalErr(err) {
			return nil, err
		}
		return softf("Codex", "Codex connected. Unable to fetch usage: %s", err.Error()), nil
	}
	if res.Status != http.StatusOK {
		return softf("Codex", "Codex connected. Usage API temporarily unavailable (%d).", res.Status), nil
	}
	data, err := decodeMap(res.Body)
	if err != nil {
		return soft("Codex", "Codex connected. Usage response was not JSON."), nil
	}
	plan := asString(data["plan_type"])
	if plan == "" {
		if summary := asMap(data["summary"]); summary != nil {
			plan = asString(summary["plan"])
		}
	}
	if plan == "" {
		plan = "unknown"
	}

	normal := firstMap(data, "rate_limit", "rate_limits")
	if normal == nil {
		if byID := asMap(data["rate_limits_by_limit_id"]); byID != nil {
			normal = asMap(byID["codex"])
		}
	}
	if normal == nil {
		normal = map[string]any{}
	}

	var quotas []Quota
	quotas = appendCodexWindows(quotas, "", normal, data)
	if review := codexReviewRateLimit(data); review != nil {
		quotas = appendCodexWindows(quotas, "review ", review, nil)
	}

	credits := 0
	if rc := asMap(data["rate_limit_reset_credits"]); rc != nil {
		credits = int(toFinite(lookupFirst(rc, "available_count", "availableCount"), 0))
		if credits < 0 {
			credits = 0
		}
	}

	return &Report{
		Plan:         plan,
		Quotas:       quotas,
		ResetCredits: credits,
		FetchedAt:    time.Now(),
	}, nil
}

func firstMap(data map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if m := asMap(data[k]); m != nil {
			return m
		}
	}
	return nil
}

func appendCodexWindows(quotas []Quota, prefix string, snapshot map[string]any, root map[string]any) []Quota {
	rate := snapshot
	if inner := asMap(snapshot["rate_limit"]); inner != nil {
		rate = inner
	}
	primary := asMap(lookupFirst(rate, "primary_window", "primary"))
	if primary == nil && root != nil {
		primary = asMap(lookupFirst(root, "primary_window", "primary"))
	}
	secondary := asMap(lookupFirst(rate, "secondary_window", "secondary"))
	if secondary == nil && root != nil {
		secondary = asMap(lookupFirst(root, "secondary_window", "secondary"))
	}
	if primary != nil {
		name := strings.TrimSpace(prefix + "session (5h)")
		quotas = append(quotas, formatCodexWindow(name, primary))
	}
	if secondary != nil {
		name := strings.TrimSpace(prefix + "weekly (7d)")
		quotas = append(quotas, formatCodexWindow(name, secondary))
	}
	return quotas
}

func formatCodexWindow(name string, window map[string]any) Quota {
	used := clampPct(toFinite(lookupFirst(window, "used_percent", "percent_used"), 0))
	remaining := clampPct(100 - used)
	return Quota{
		Name:         name,
		Used:         used,
		Total:        100,
		Remaining:    remaining,
		RemainingPct: pctPtr(remaining),
		ResetAt:      parseResetTime(lookupFirst(window, "reset_at", "resets_at", "resetAt")),
	}
}

func codexReviewRateLimit(data map[string]any) map[string]any {
	if m := asMap(lookupFirst(data, "code_review_rate_limit", "review_rate_limit")); m != nil {
		return m
	}
	if byID := asMap(data["rate_limits_by_limit_id"]); byID != nil {
		if m := asMap(lookupFirst(byID, "code_review", "codex_review", "review")); m != nil {
			return m
		}
	}
	if extra, ok := data["additional_rate_limits"].([]any); ok {
		for _, e := range extra {
			m := asMap(e)
			if m == nil {
				continue
			}
			id := strings.ToLower(asString(lookupFirst(m, "limit_name", "metered_feature", "id")))
			if id == "code_review" || id == "codex_review" || id == "review" || strings.Contains(id, "review") {
				return m
			}
		}
	}
	return nil
}
