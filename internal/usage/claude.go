package usage

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"airouter/internal/domain"
)

func (s *Service) fetchClaude(ctx context.Context, p *domain.Provider) (*Report, error) {
	if msg := s.claudeCooldownMessage(p.ID); msg != nil {
		return msg, nil
	}
	headers := map[string]string{
		"anthropic-beta":    "oauth-2025-04-20",
		"anthropic-version": "2023-06-01",
	}
	res, _, err := s.doAuthed(ctx, p, http.MethodGet, ClaudeUsageURL, headers, nil)
	if err != nil {
		if isLocalErr(err) {
			return nil, err
		}
		return softf("Claude Code", "Claude connected. Unable to fetch usage: %s", err.Error()), nil
	}
	if res.Status == http.StatusTooManyRequests {
		s.setClaudeCooldown(p.ID)
		return soft("Claude Code", "Claude Code connected. Quota API rate-limited (429). Chat may still work."), nil
	}
	if res.Status != http.StatusOK {
		return softf("Claude Code", "Claude connected. Quota API unavailable (%d). Chat may still work.", res.Status), nil
	}
	data, err := decodeMap(res.Body)
	if err != nil {
		return soft("Claude Code", "Claude connected. Usage response was not JSON."), nil
	}
	var quotas []Quota
	if q, ok := claudeWindow("session (5h)", asMap(data["five_hour"])); ok {
		quotas = append(quotas, q)
	}
	if q, ok := claudeWindow("weekly (7d)", asMap(data["seven_day"])); ok {
		quotas = append(quotas, q)
	}
	var extras []Quota
	for key, value := range data {
		if !strings.HasPrefix(key, "seven_day_") || key == "seven_day" {
			continue
		}
		model := strings.TrimPrefix(key, "seven_day_")
		if q, ok := claudeWindow("weekly "+model+" (7d)", asMap(value)); ok {
			extras = append(extras, q)
		}
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
	quotas = append(quotas, extras...)
	return &Report{
		Plan:      "Claude Code",
		Quotas:    quotas,
		FetchedAt: time.Now(),
	}, nil
}

func claudeWindow(name string, window map[string]any) (Quota, bool) {
	if window == nil {
		return Quota{}, false
	}
	used, ok := asFloat(window["utilization"])
	if !ok {
		return Quota{}, false
	}
	used = clampPct(used)
	remaining := clampPct(100 - used)
	return Quota{
		Name:         name,
		Used:         used,
		Total:        100,
		Remaining:    remaining,
		RemainingPct: pctPtr(remaining),
		ResetAt:      parseResetTime(window["resets_at"]),
	}, true
}
