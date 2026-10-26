package usage

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
)

// qoderNeverExpiresSentinel is Qoder's "never expires" reset value: an
// absolute timestamp in milliseconds at 9999-12-31T00:00:00Z, returned by
// both regions for open-ended plans.
const qoderNeverExpiresSentinel = 253402214400000

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
	resetAt, neverExpires := qoderExpiry(data["expiresAt"])
	var quotas []Quota
	if q, ok := qoderBucket("Personal", asMap(data["userQuota"]), resetAt, neverExpires); ok {
		quotas = append(quotas, q)
	}
	if org := asMap(data["orgResourcePackage"]); org != nil {
		if toFinite(org["total"], 0) != 0 {
			if q, ok := qoderBucket("Organization", org, resetAt, neverExpires); ok {
				quotas = append(quotas, q)
			}
		}
	}
	// Add-on credits (free grants, promos). Qoder emits the block only when
	// the account holds a grant, so gating on a positive total keeps the
	// no-grant shape unchanged.
	if addOn := asMap(data["addOnQuota"]); addOn != nil && toFinite(addOn["total"], 0) > 0 {
		// The add-on block carries no expiry of its own in this API, and the
		// account-level sentinel describes the plan, not the grants. Leave
		// both reset fields unset so a per-grant deadline is never fabricated.
		quotas = append(quotas, qoderBucketValues("Add-on", addOn, nil, false))
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

// qoderExpiry reads the account-level expiresAt. The sentinel means the plan
// never expires; any other finite value is a real reset time; anything
// unparsable stays unknown. Global time parsing is not changed.
func qoderExpiry(v any) (resetAt *time.Time, neverExpires bool) {
	if qoderMillis(v) == qoderNeverExpiresSentinel {
		return nil, true
	}
	return parseResetTime(v), false
}

// qoderMillis accepts the sentinel as a number or an all-digits string.
func qoderMillis(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		if !isAllDigits(strings.TrimSpace(t)) {
			return 0
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func qoderBucket(name string, raw map[string]any, resetAt *time.Time, neverExpires bool) (Quota, bool) {
	if raw == nil {
		return Quota{}, false
	}
	return qoderBucketValues(name, raw, resetAt, neverExpires), true
}

func qoderBucketValues(name string, raw map[string]any, resetAt *time.Time, neverExpires bool) Quota {
	used := toFinite(raw["used"], 0)
	total := toFinite(raw["total"], 0)
	remaining := toFinite(raw["remaining"], total-used)
	unit := asString(raw["unit"])
	if unit == "" {
		unit = "credits"
	}
	return Quota{
		Name:         name,
		Used:         used,
		Total:        total,
		Remaining:    remaining,
		ResetAt:      resetAt,
		NeverExpires: neverExpires,
		Unit:         unit,
	}
}
