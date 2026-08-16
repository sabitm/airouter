package usage

import (
	"context"
	"net/http"
	"time"

	"airouter/internal/domain"
)

func (s *Service) fetchQoder(ctx context.Context, p *domain.Provider) (*Report, error) {
	res, _, err := s.doAuthed(ctx, p, http.MethodGet, QoderUsageURL, nil, nil)
	if err != nil {
		if isLocalErr(err) {
			return nil, err
		}
		return softf("Qoder", "Qoder connected. Unable to fetch usage: %s", err.Error()), nil
	}
	if res.Status != http.StatusOK {
		return softf("Qoder", "Qoder connected. Usage fetch returned %d.", res.Status), nil
	}
	data, err := decodeMap(res.Body)
	if err != nil {
		return soft("Qoder", "Qoder connected. Usage response was not JSON."), nil
	}
	resetAt := parseResetTime(data["expiresAt"])
	var quotas []Quota
	if q, ok := qoderBucket("Personal", asMap(data["userQuota"]), resetAt); ok {
		quotas = append(quotas, q)
	}
	if org := asMap(data["orgResourcePackage"]); org != nil {
		if toFinite(org["total"], 0) != 0 {
			if q, ok := qoderBucket("Organization", org, resetAt); ok {
				quotas = append(quotas, q)
			}
		}
	}
	msg := ""
	if len(quotas) == 0 {
		msg = "Qoder connected. No quota buckets reported."
	}
	return &Report{
		Plan:      "Qoder",
		Message:   msg,
		Quotas:    quotas,
		FetchedAt: time.Now(),
	}, nil
}

func qoderBucket(name string, raw map[string]any, resetAt *time.Time) (Quota, bool) {
	if raw == nil {
		return Quota{}, false
	}
	used := toFinite(raw["used"], 0)
	total := toFinite(raw["total"], 0)
	remaining := toFinite(raw["remaining"], total-used)
	unit := asString(raw["unit"])
	if unit == "" {
		unit = "credits"
	}
	return Quota{
		Name:      name,
		Used:      used,
		Total:     total,
		Remaining: remaining,
		ResetAt:   resetAt,
		Unit:      unit,
	}, true
}
