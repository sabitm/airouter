package web

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"airouter/internal/domain"
	"airouter/internal/usage"
)

const usageUntaggedFilter = "__untagged__"

func usageSupportedProviders(all []*domain.Provider) []*domain.Provider {
	out := make([]*domain.Provider, 0, len(all))
	for _, p := range all {
		if p == nil || p.Archived || !usage.Supported(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// parseUsageTagFilter accepts All (empty), Untagged, or one exact tag present
// on the supported set. Unknown values fall back to All.
func parseUsageTagFilter(raw string, supported []*domain.Provider) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw == usageUntaggedFilter {
		if domain.HasUntagged(supported) {
			return usageUntaggedFilter
		}
		return ""
	}
	for _, t := range domain.UniqueTags(supported) {
		if t == raw {
			return t
		}
	}
	return ""
}

func filterUsageProviders(providers []*domain.Provider, filter string) []*domain.Provider {
	if filter == "" {
		return providers
	}
	out := make([]*domain.Provider, 0, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		if filter == usageUntaggedFilter {
			if len(p.Tags) == 0 {
				out = append(out, p)
			}
			continue
		}
		if p.HasTag(filter) {
			out = append(out, p)
		}
	}
	return out
}

func usageLoadAllURL(filter string) string {
	if filter == "" {
		return "/dashboard/usage?load=1"
	}
	return "/dashboard/usage?tag=" + url.QueryEscape(filter) + "&load=1"
}

func usageFilterURL(filter string) string {
	if filter == "" {
		return "/dashboard/usage"
	}
	return "/dashboard/usage?tag=" + url.QueryEscape(filter)
}

func usageCardURL(id int64, force bool) string {
	u := "/dashboard/usage/card/" + strconv.FormatInt(id, 10)
	if force {
		return u + "?force=1"
	}
	return u
}

func usageCodexResetURL(id int64) string {
	return "/dashboard/usage/card/" + strconv.FormatInt(id, 10) + "/codex-reset"
}

func remainingPct(q usage.Quota) float64 {
	if q.RemainingPct != nil {
		return clamp01to100(*q.RemainingPct)
	}
	if q.Total > 0 {
		return clamp01to100(((q.Total - q.Used) / q.Total) * 100)
	}
	return 0
}

func clamp01to100(n float64) float64 {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func quotaBarClass(q usage.Quota) string {
	pct := remainingPct(q)
	switch {
	case pct > 70:
		return "quota-ok"
	case pct >= 30:
		return "quota-warn"
	default:
		return "quota-low"
	}
}

func quotaBarStyle(q usage.Quota) templ.SafeCSS {
	width := remainingPct(q)
	return templ.SafeCSS(fmt.Sprintf("width: %.1f%%", width))
}

func quotaUsedText(q usage.Quota) string {
	pct := remainingPct(q)
	if quotaPctOnly(q) {
		return fmt.Sprintf("%.0f%% remaining", pct)
	}
	used := formatQuotaNum(q.Used)
	total := formatQuotaNum(q.Total)
	unit := q.Unit
	if unit != "" {
		return fmt.Sprintf("%s/%s %s (%.0f%% remaining)", used, total, unit, pct)
	}
	return fmt.Sprintf("%s/%s (%.0f%% remaining)", used, total, pct)
}

func quotaMeta(q usage.Quota) string {
	if q.Unlimited {
		return "unlimited"
	}
	return ""
}

func quotaPctOnly(q usage.Quota) bool {
	return q.RemainingPct != nil && q.Total == 0 && q.Used == 0
}

func formatQuotaNum(n float64) string {
	if n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'f', 1, 64)
}

func formatResetCountdown(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	diff := time.Until(*t)
	if diff <= 0 {
		return "-"
	}
	totalMin := int((diff + time.Minute - 1) / time.Minute)
	if totalMin < 60 {
		return fmt.Sprintf("%dm", totalMin)
	}
	hours := totalMin / 60
	mins := totalMin % 60
	if hours < 24 {
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := hours / 24
	remH := hours % 24
	if remH == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, remH)
}

func usageErrMessage(p *domain.Provider, err error) string {
	if err == nil {
		return ""
	}
	name := "Provider"
	if p != nil && p.Name != "" {
		name = p.Name
	}
	switch err {
	case usage.ErrUnsupported:
		return name + " has no quota API."
	case usage.ErrNoToken:
		return name + " has no access token. Reconnect or add a key."
	default:
		return name + ": " + err.Error()
	}
}
