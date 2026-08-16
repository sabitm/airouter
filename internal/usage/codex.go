package usage

import (
	"context"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
)

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
