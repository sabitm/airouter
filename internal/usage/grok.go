package usage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"airouter/internal/domain"
)

// Grok CLI xAI upstream identity captured from official grok-shell 0.2.99.
const (
	grokUserAgent   = "grok-shell/0.2.99 (linux; x86_64)"
	grokClientIdent = "grok-shell"
	grokClientVer   = "0.2.99"
	grokTokenAuth   = "xai-grok-cli"
)

const grokPlan = "Grok"

func grokHeaders(email string) map[string]string {
	h := map[string]string{
		"Accept":                   "application/json",
		"User-Agent":               grokUserAgent,
		"x-xai-token-auth":         grokTokenAuth,
		"x-grok-client-identifier": grokClientIdent,
		"x-grok-client-version":    grokClientVer,
		"x-grok-client-mode":       "headless",
	}
	if email != "" {
		h["x-email"] = email
	}
	return h
}

func (s *Service) fetchGrok(ctx context.Context, p *domain.Provider) (*Report, error) {
	token, err := s.resolveToken(ctx, p, false)
	if err != nil {
		return nil, err
	}

	email := ""
	if p.OAuthCreds != nil {
		email = strings.TrimSpace(p.OAuthCreds.Email)
	}

	billingRes, billingErr := s.doJSON(ctx, http.MethodGet, GrokBillingURL, token, grokHeaders(email), nil)
	userRes, userErr := s.doJSON(ctx, http.MethodGet, GrokUserURL, token, grokHeaders(email), nil)

	// Billing is authoritative. Refresh once on auth rejection before giving up.
	if billingErr == nil && (billingRes.Status == http.StatusUnauthorized || billingRes.Status == http.StatusForbidden) {
		refreshed, rerr := s.resolveToken(ctx, p, true)
		if rerr != nil && isLocalErr(rerr) {
			return nil, rerr
		}
		if rerr == nil && refreshed != "" {
			token = refreshed
			billingRes, billingErr = s.doJSON(ctx, http.MethodGet, GrokBillingURL, token, grokHeaders(email), nil)
			if billingErr == nil {
				userRes, userErr = s.doJSON(ctx, http.MethodGet, GrokUserURL, token, grokHeaders(email), nil)
			}
		}
	}

	// A transport failure on billing is not a hard auth error.
	if billingErr != nil {
		if isLocalErr(billingErr) {
			return nil, billingErr
		}
		return softf(grokPlan, "Grok connected. Unable to fetch usage: %s", billingErr.Error()), nil
	}
	if billingRes.Status == http.StatusUnauthorized || billingRes.Status == http.StatusForbidden {
		return soft(grokPlan, "Grok CLI authentication expired. Please re-authorize."), nil
	}
	if billingRes.Status != http.StatusOK {
		return softf(grokPlan, "Grok connected. Usage API unavailable (%d).", billingRes.Status), nil
	}

	billing, err := decodeMap(billingRes.Body)
	if err != nil {
		return soft(grokPlan, "Grok connected. Usage response was not JSON."), nil
	}

	// The user profile only enriches plan and subscription state; a failure here
	// must never discard billing rows.
	var user map[string]any
	if userErr == nil && userRes.Status == http.StatusOK {
		user, _ = decodeMap(userRes.Body)
	}

	parsed := parseGrokBilling(billing, user)
	plan := grokPlanFromToken(token)
	if plan != "" {
		parsed.plan = plan
	} else if parsed.plan == "" {
		parsed.plan = grokPlan
	}

	if len(parsed.quotas) == 0 {
		// Paid subscriptions often expose cap=0 over REST but publish the shared
		// weekly pool on gRPC-web GetGrokCreditsConfig.
		if parsed.subscriptionAccess {
			if decoded, ok := s.fetchGrokCredits(ctx, token); ok {
				return &Report{
					Plan:      parsed.plan,
					Quotas:    []Quota{grokGrpcQuota(decoded)},
					FetchedAt: time.Now(),
				}, nil
			}
			return soft(parsed.plan, "Subscription access is active; Grok does not expose a numeric included quota."), nil
		}
		return &Report{
			Plan:      parsed.plan,
			Message:   "Grok Build connected, but no credit allotment was returned. Free promo may be exhausted.",
			FetchedAt: time.Now(),
		}, nil
	}

	return &Report{Plan: parsed.plan, Quotas: parsed.quotas, FetchedAt: time.Now()}, nil
}

type grokParsed struct {
	plan               string
	quotas             []Quota
	subscriptionAccess bool
}

// unwrapGrokVal unwraps protobuf-json `{ val: n }` or plain numbers/strings.
func unwrapGrokVal(v any, fallback float64) float64 {
	if m := asMap(v); m != nil {
		if n, ok := m["val"]; ok {
			return toFinite(n, fallback)
		}
	}
	return toFinite(v, fallback)
}

func grokSubscriptionTier(user, config map[string]any) string {
	raw := asString(lookupFirst(user,
		"subscriptionTier", "subscription_tier"))
	if raw == "" {
		if sub := asMap(user["subscription"]); sub != nil {
			raw = asString(sub["tier"])
		}
	}
	if raw == "" {
		raw = asString(lookupFirst(config, "subscriptionTier", "subscription_tier"))
	}
	return raw
}

func grokResolvePlan(user, config map[string]any) string {
	if tier := grokSubscriptionTier(user, config); tier != "" {
		return grokTitleCase(tier)
	}
	if isTruthy(user["hasGrokCodeAccess"]) {
		return "Grok Code"
	}
	if isTruthy(config["isUnifiedBillingUser"]) {
		return "Grok Build"
	}
	return "Grok Build"
}

var grokWordRE = regexp.MustCompile(`\b\w`)

func grokTitleCase(s string) string {
	s = strings.NewReplacer("_", " ", "-", " ").Replace(s)
	return grokWordRE.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ToUpper(m)
	})
}

// grokPlanFromToken decodes the JWT access-token tier claim for display only.
// Upstream remains authoritative for access and quota enforcement; this value is
// never used for authorization.
func grokPlanFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	tier := toFinite(claims["tier"], -1)
	switch int64(tier) {
	case 0:
		return "Free"
	case 1:
		return "SuperGrok"
	case 2:
		return "X Basic"
	case 3:
		return "X Premium"
	case 4:
		return "X Premium Plus"
	case 5:
		return "SuperGrok Heavy"
	case 6:
		return "SuperGrok Lite"
	default:
		return ""
	}
}

// parseGrokBilling maps billing JSON (plus optional user profile) into quota
// rows following Grok CLI's wire shape.
func parseGrokBilling(billing, user map[string]any) grokParsed {
	root := billing
	config := asMap(root["config"])
	if config == nil {
		config = root
	}

	resetAt := parseResetTime(lookupFirst(config,
		"billingPeriodEnd", "billing_period_end"))
	if resetAt == nil {
		if cur := asMap(config["currentPeriod"]); cur != nil {
			resetAt = parseResetTime(cur["end"])
		}
	}
	if resetAt == nil {
		resetAt = parseResetTime(lookupFirst(config, "resetAt", "resetsAt", "periodEnd"))
	}
	if resetAt == nil {
		resetAt = parseResetTime(lookupFirst(root,
			"billingPeriodEnd", "billing_period_end", "resetAt", "resetsAt", "periodEnd"))
	}

	tier := grokSubscriptionTier(user, config)
	subscriptionAccess := tier != "" && !grokIsFreeTier(tier)

	quotas := []Quota{}

	// Monthly included usage, reported at top level by current Grok Build.
	monthlyLimit := unwrapGrokVal(lookupFirst(config, "monthlyLimit", "monthly_limit"), math.NaN())
	if math.IsNaN(monthlyLimit) {
		monthlyLimit = unwrapGrokVal(lookupFirst(root, "monthlyLimit", "monthly_limit"), math.NaN())
	}
	includedUsed := unwrapGrokVal(lookupFirst(config, "includedUsed", "included_used"), math.NaN())
	if math.IsNaN(includedUsed) {
		includedUsed = unwrapGrokVal(lookupFirst(root, "includedUsed", "included_used"), math.NaN())
	}
	totalUsed := unwrapGrokVal(lookupFirst(config, "totalUsed", "total_used"), math.NaN())
	if math.IsNaN(totalUsed) {
		totalUsed = unwrapGrokVal(lookupFirst(root, "totalUsed", "total_used"), math.NaN())
	}
	if !math.IsNaN(monthlyLimit) && !math.IsInf(monthlyLimit, 0) && monthlyLimit > 0 {
		used := includedUsed
		if math.IsNaN(used) {
			used = totalUsed
		}
		if math.IsNaN(used) {
			used = 0
		}
		quotas = append(quotas, grokQuota("Monthly included", used, monthlyLimit, resetAt, false))
	}

	// On-demand spending window.
	onDemandCap := unwrapGrokVal(lookupFirst(config, "onDemandCap"), math.NaN())
	if math.IsNaN(onDemandCap) {
		onDemandCap = unwrapGrokVal(lookupFirst(root, "onDemandCap"), math.NaN())
	}
	onDemandUsed := unwrapGrokVal(lookupFirst(config, "onDemandUsed"), math.NaN())
	if math.IsNaN(onDemandUsed) {
		onDemandUsed = unwrapGrokVal(lookupFirst(root, "onDemandUsed"), math.NaN())
	}
	if !math.IsNaN(onDemandCap) && !math.IsInf(onDemandCap, 0) && onDemandCap > 0 {
		used := onDemandUsed
		if math.IsNaN(used) || used < 0 {
			used = 0
		}
		quotas = append(quotas, grokQuota("On-demand", used, onDemandCap, resetAt, false))
	} else if !subscriptionAccess && !math.IsNaN(onDemandCap) && !math.IsInf(onDemandCap, 0) && onDemandCap == 0 && !math.IsNaN(onDemandUsed) && !math.IsInf(onDemandUsed, 0) {
		// Cap 0 with numeric used on a non-paid tier is the exhausted free/promo
		// state; render a synthetic depleted bar (total===0 reads as unlimited).
		quotas = append(quotas, Quota{Name: "On-demand", Used: 1, Total: 1, Remaining: 0, RemainingPct: pctPtr(0), ResetAt: resetAt})
	}

	// Prepaid top-up balance is a remaining pot; show full bar (0 spent).
	prepaid := unwrapGrokVal(lookupFirst(config, "prepaidBalance"), math.NaN())
	if math.IsNaN(prepaid) {
		prepaid = unwrapGrokVal(lookupFirst(root, "prepaidBalance"), math.NaN())
	}
	if !math.IsNaN(prepaid) && !math.IsInf(prepaid, 0) && prepaid > 0 {
		quotas = append(quotas, Quota{Name: "Prepaid", Used: 0, Total: prepaid, Remaining: prepaid, RemainingPct: pctPtr(100)})
	}

	// SuperGrok weekly shared-pool usage percentage.
	usedPct := unwrapGrokVal(lookupFirst(config, "creditUsagePercent", "credit_usage_percent"), math.NaN())
	if math.IsNaN(usedPct) {
		usedPct = unwrapGrokVal(lookupFirst(root, "creditUsagePercent", "credit_usage_percent"), math.NaN())
	}
	if !math.IsNaN(usedPct) && !math.IsInf(usedPct, 0) && usedPct >= 0 {
		used := math.Max(0, math.Min(100, usedPct))
		quotas = append(quotas, grokQuota("Weekly SuperGrok", used, 100, resetAt, false))
	}

	// Opportunistic generic credit bags. productUsage is a breakdown legend and
	// must never become rows.
	for _, bag := range []any{
		root["credits"], root["creditBalance"], root["usage"],
		config["credits"], config["includedCredits"], config["subscriptionCredits"],
	} {
		bm := asMap(bag)
		if bm == nil {
			continue
		}
		if grokHasCreditRow(quotas) {
			break
		}
		total := unwrapGrokVal(lookupFirst(bm, "total", "limit", "cap", "allocation", "amount"), math.NaN())
		used := unwrapGrokVal(lookupFirst(bm, "used", "spent", "consumed"), math.NaN())
		remaining := unwrapGrokVal(lookupFirst(bm, "remaining", "balance", "left"), math.NaN())
		if !math.IsNaN(total) && !math.IsInf(total, 0) && total > 0 {
			resolvedUsed := used
			if math.IsNaN(resolvedUsed) {
				if !math.IsNaN(remaining) && !math.IsInf(remaining, 0) {
					resolvedUsed = math.Max(0, total-remaining)
				} else {
					resolvedUsed = 0
				}
			}
			bagReset := parseResetTime(lookupFirst(bm, "resetAt", "resetsAt", "end"))
			if bagReset == nil {
				bagReset = resetAt
			}
			quotas = append(quotas, grokQuota("Credits", resolvedUsed, total, bagReset, false))
		} else if !math.IsNaN(remaining) && !math.IsInf(remaining, 0) && remaining >= 0 {
			q := Quota{Name: "Credits", ResetAt: resetAt}
			if remaining > 0 {
				q.Total = remaining
				q.Remaining = remaining
				q.RemainingPct = pctPtr(100)
			} else {
				q.Total = 1
				q.Remaining = 0
				q.RemainingPct = pctPtr(0)
			}
			quotas = append(quotas, q)
		}
	}

	return grokParsed{
		plan:               grokResolvePlan(user, config),
		quotas:             quotas,
		subscriptionAccess: subscriptionAccess,
	}
}

func grokIsFreeTier(tier string) bool {
	switch strings.ToLower(tier) {
	case "free", "none", "null":
		return true
	default:
		return false
	}
}

func grokHasCreditRow(quotas []Quota) bool {
	for _, q := range quotas {
		if q.Name == "Credits" {
			return true
		}
	}
	return false
}

func grokQuota(name string, used, total float64, resetAt *time.Time, unlimited bool) Quota {
	if unlimited || total <= 0 {
		return Quota{
			Name:  name,
			Used:  math.Max(0, used),
			Total: 0,
			RemainingPct: pctPtr(func() float64 {
				if unlimited {
					return 100
				}
				return 0
			}()),
			ResetAt:   resetAt,
			Unlimited: unlimited,
		}
	}
	safeUsed := math.Max(0, used)
	remaining := math.Max(0, total-safeUsed)
	return Quota{
		Name:         name,
		Used:         safeUsed,
		Total:        total,
		Remaining:    remaining,
		RemainingPct: pctPtr((remaining / total) * 100),
		ResetAt:      resetAt,
	}
}

func grokGrpcQuota(d grokCreditsDecoded) Quota {
	// Round for bar display (fixed32 ratio * 100 can be 34.999… for 0.35).
	used := math.Round(clampPct(d.percentUsed))
	return grokQuota("Weekly SuperGrok", used, 100, d.resetAt, false)
}
