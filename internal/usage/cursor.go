package usage

import (
	"context"
	"encoding/binary"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/cursor"
)

const (
	cursorPlan                  = "Cursor"
	cursorUnavailableMessage    = "Usage details are not available for this plan in the CLI."
	cursorEnterpriseUnavailable = "Enterprise spend details are not available for this account in the CLI."
	cursorDashboardService      = "/aiserver.v1.DashboardService/"
	cursorMethodPeriodUsage     = "GetCurrentPeriodUsage"
	cursorMethodHardLimit       = "GetHardLimit"
	cursorMethodPlanInfo        = "GetPlanInfo"
	cursorMethodGetMe           = "GetMe"
	cursorHardLimitUnlimited    = int32(2147483647)
	cursorRPCTimeout            = 10 * time.Second
)

const (
	cursorWireVarint  = 0
	cursorWireFixed64 = 1
	cursorWireLen     = 2
	cursorWireFixed32 = 5
)

func (s *Service) fetchCursor(ctx context.Context, p *domain.Provider) (*Report, error) {
	token, err := s.resolveToken(ctx, p, false)
	if err != nil {
		return nil, err
	}

	periodRes, periodErr, hardRes, hardErr, planRes, planErr := s.fetchCursorPeriodBundle(ctx, p, token)
	if periodErr == nil && (periodRes.Status == http.StatusUnauthorized || periodRes.Status == http.StatusForbidden) && p.Method() == domain.AuthOAuth {
		refreshed, rerr := s.resolveToken(ctx, p, true)
		if rerr != nil && isLocalErr(rerr) {
			return nil, rerr
		}
		if rerr == nil && refreshed != "" {
			token = refreshed
			periodRes, periodErr, hardRes, hardErr, planRes, planErr = s.fetchCursorPeriodBundle(ctx, p, token)
		}
	}

	if periodErr != nil {
		if isLocalErr(periodErr) {
			return nil, periodErr
		}
		return softf(cursorPlan, "Cursor connected. Unable to fetch usage: %s", periodErr.Error()), nil
	}
	if periodRes.Status == http.StatusUnauthorized || periodRes.Status == http.StatusForbidden {
		return soft(cursorPlan, "Cursor authentication expired. Please re-authorize."), nil
	}
	if periodRes.Status != http.StatusOK {
		return softf(cursorPlan, "Cursor connected. Usage API unavailable (%d).", periodRes.Status), nil
	}

	period, ok := decodeCursorPeriodUsage(periodRes.Body)
	if !ok {
		return soft(cursorPlan, "Cursor connected. Usage response could not be decoded."), nil
	}

	var hard *cursorHardLimit
	if hardErr == nil && hardRes.Status == http.StatusOK {
		if decoded, decodedOK := decodeCursorHardLimit(hardRes.Body); decodedOK {
			hard = &decoded
		}
	}
	var planInfo *cursorPlanInfo
	if planErr == nil && planRes.Status == http.StatusOK {
		if decoded, decodedOK := decodeCursorPlanInfo(planRes.Body); decodedOK {
			planInfo = &decoded
		}
	}

	if period.planUsage == nil {
		return s.fetchCursorEnterpriseOrUnavailable(ctx, p, token, planInfo)
	}
	return buildCursorStandardReport(period, hard, planInfo), nil
}

func (s *Service) fetchCursorPeriodBundle(ctx context.Context, p *domain.Provider, token string) (httpResult, error, httpResult, error, httpResult, error) {
	type slot struct {
		res httpResult
		err error
	}
	periodCh := make(chan slot, 1)
	hardCh := make(chan slot, 1)
	planCh := make(chan slot, 1)
	go func() {
		res, err := s.doCursorRPC(ctx, p, token, cursorMethodPeriodUsage)
		periodCh <- slot{res, err}
	}()
	go func() {
		res, err := s.doCursorRPC(ctx, p, token, cursorMethodHardLimit)
		hardCh <- slot{res, err}
	}()
	go func() {
		res, err := s.doCursorRPC(ctx, p, token, cursorMethodPlanInfo)
		planCh <- slot{res, err}
	}()
	period := <-periodCh
	hard := <-hardCh
	plan := <-planCh
	return period.res, period.err, hard.res, hard.err, plan.res, plan.err
}

func (s *Service) fetchCursorEnterpriseOrUnavailable(ctx context.Context, p *domain.Provider, token string, planInfo *cursorPlanInfo) (*Report, error) {
	meRes, meErr := s.doCursorRPC(ctx, p, token, cursorMethodGetMe)
	if meErr != nil || meRes.Status != http.StatusOK {
		return soft(cursorPlan, cursorUnavailableMessage), nil
	}
	me, ok := decodeCursorMe(meRes.Body)
	if !ok || !me.isEnterpriseUser {
		return soft(cursorPlan, cursorUnavailableMessage), nil
	}
	plan := "Enterprise"
	if planInfo != nil && strings.TrimSpace(planInfo.planName) != "" {
		plan = strings.TrimSpace(planInfo.planName)
	}
	if me.teamID == 0 || me.userID == 0 {
		return soft(plan, cursorEnterpriseUnavailable), nil
	}
	return soft(plan, cursorEnterpriseUnavailable), nil
}

func buildCursorStandardReport(period cursorPeriodUsage, hard *cursorHardLimit, planInfo *cursorPlanInfo) *Report {
	plan := "Current plan"
	if planInfo != nil && strings.TrimSpace(planInfo.planName) != "" {
		plan = strings.TrimSpace(planInfo.planName)
	}

	resetAt := unixMillisToTime(period.billingCycleEnd)
	if resetAt == nil && planInfo != nil {
		resetAt = unixMillisToTime(planInfo.billingCycleEnd)
	}

	quotas := []Quota{cursorIncludedQuota(includedPercentUsed(period.planUsage), resetAt)}
	msg := ""
	if q, ok, note := cursorOnDemandQuota(period.spendLimit, hard, resetAt); ok {
		quotas = append(quotas, q)
	} else if note != "" && len(quotas) == 0 {
		msg = note
	}

	return &Report{
		Plan:      plan,
		Message:   msg,
		Quotas:    quotas,
		FetchedAt: time.Now(),
	}
}

func includedPercentUsed(pu *cursorPlanUsage) float64 {
	if pu == nil {
		return 0
	}
	if pu.totalPercentUsed != nil {
		return clampPct(*pu.totalPercentUsed)
	}
	if pu.limit > 0 {
		included := pu.includedSpend
		if included < 0 {
			included = 0
		}
		return clampPct(float64(included) / float64(pu.limit) * 100)
	}
	return 0
}

func cursorIncludedQuota(percentUsed float64, resetAt *time.Time) Quota {
	return Quota{
		Name:         "Included",
		RemainingPct: pctPtr(100 - clampPct(percentUsed)),
		ResetAt:      resetAt,
	}
}

func cursorOnDemandQuota(spend *cursorSpendLimit, hard *cursorHardLimit, resetAt *time.Time) (Quota, bool, string) {
	usedDollars := 0.0
	if spend != nil {
		used := spend.individualUsed
		if used < 0 {
			used = 0
		}
		usedDollars = float64(used) / 100
	}

	kind, limitDollars := cursorOnDemandKind(spend, hard)
	switch kind {
	case "fixed":
		if limitDollars <= 0 {
			return Quota{}, false, "On-demand usage is disabled."
		}
		remaining := math.Max(0, limitDollars-usedDollars)
		return Quota{
			Name:         "On-demand",
			Used:         usedDollars,
			Total:        limitDollars,
			Remaining:    remaining,
			RemainingPct: pctPtr((remaining / limitDollars) * 100),
			ResetAt:      resetAt,
			Unit:         "$",
		}, true, ""
	case "disabled":
		return Quota{}, false, "On-demand usage is disabled."
	case "unavailable":
		return Quota{}, false, "On-demand usage is unavailable."
	default:
		return Quota{}, false, ""
	}
}

func cursorOnDemandKind(spend *cursorSpendLimit, hard *cursorHardLimit) (kind string, limitDollars float64) {
	var individualLimit *int32
	limitType := ""
	var pooledLimit *int32
	if spend != nil {
		individualLimit = spend.individualLimit
		limitType = spend.limitType
		pooledLimit = spend.pooledLimit
	}

	fixedFromIndividual := func() (string, float64, bool) {
		if individualLimit == nil {
			return "", 0, false
		}
		if *individualLimit > 0 {
			return "fixed", float64(*individualLimit) / 100, true
		}
		return "disabled", 0, true
	}

	if limitType == "team" {
		if kind, dollars, ok := fixedFromIndividual(); ok {
			return kind, dollars
		}
		if hard != nil {
			if hard.noUsageBasedAllowed || hard.hardLimit <= 0 {
				return "disabled", 0
			}
			return "unlimited", 0
		}
		if pooledLimit != nil && *pooledLimit > 0 {
			return "unlimited", 0
		}
		return "unavailable", 0
	}

	if hard == nil {
		if kind, dollars, ok := fixedFromIndividual(); ok {
			return kind, dollars
		}
		return "unavailable", 0
	}
	if hard.noUsageBasedAllowed {
		return "disabled", 0
	}
	if hard.hardLimit >= cursorHardLimitUnlimited {
		return "unlimited", 0
	}
	if hard.hardLimit > 0 {
		return "fixed", float64(hard.hardLimit)
	}
	return "disabled", 0
}

func (s *Service) doCursorRPC(ctx context.Context, p *domain.Provider, token, method string) (httpResult, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, cursorRPCTimeout)
	defer cancel()
	headers := cursorUsageHeaders(token)
	rawURL := cursorDashboardURL(p, method)
	return s.doRequest(rpcCtx, http.MethodPost, rawURL, "", headers, []byte{})
}

func cursorUsageHeaders(token string) map[string]string {
	h := cursor.BuildDashboardHeaders(token, true)
	out := make(map[string]string, len(h))
	for k, vv := range h {
		if len(vv) == 0 {
			continue
		}
		out[k] = vv[0]
	}
	out["Accept"] = cursor.ProtoContentType
	out["Content-Type"] = cursor.ProtoContentType
	out["Connect-Timeout-Ms"] = strconv.FormatInt(cursorRPCTimeout.Milliseconds(), 10)
	return out
}

func cursorDashboardURL(p *domain.Provider, method string) string {
	base := strings.TrimRight(strings.TrimSpace(CursorDashboardBaseURL), "/")
	if p != nil {
		if b := strings.TrimSpace(p.BaseURL); b != "" && cursorBaseUsable(b) {
			base = strings.TrimRight(b, "/")
		}
	}
	return base + cursorDashboardService + method
}

func cursorBaseUsable(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "agent.api5.cursor.sh" || strings.HasSuffix(host, ".api5.cursor.sh") {
		return false
	}
	return true
}

func unixMillisToTime(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

type cursorPeriodUsage struct {
	billingCycleEnd int64
	planUsage       *cursorPlanUsage
	spendLimit      *cursorSpendLimit
}

type cursorPlanUsage struct {
	includedSpend    int32
	limit            int32
	totalPercentUsed *float64
}

type cursorSpendLimit struct {
	individualUsed  int32
	individualLimit *int32
	pooledLimit     *int32
	limitType       string
}

type cursorHardLimit struct {
	hardLimit           int32
	noUsageBasedAllowed bool
}

type cursorPlanInfo struct {
	planName        string
	billingCycleEnd int64
}

type cursorMe struct {
	userID           int32
	teamID           int32
	isEnterpriseUser bool
}

func decodeCursorPeriodUsage(body []byte) (cursorPeriodUsage, bool) {
	fields, ok := decodeCursorFields(body)
	if !ok {
		return cursorPeriodUsage{}, false
	}
	out := cursorPeriodUsage{}
	if v, present, valid := cursorInt64(fields, 2); !valid && present {
		return cursorPeriodUsage{}, false
	} else if present {
		out.billingCycleEnd = v
	}
	if nested, present := cursorNested(fields, 3); present {
		if nested == nil {
			return cursorPeriodUsage{}, false
		}
		pu, ok := decodeCursorPlanUsage(nested)
		if !ok {
			return cursorPeriodUsage{}, false
		}
		out.planUsage = &pu
	}
	if nested, present := cursorNested(fields, 4); present {
		if nested == nil {
			return cursorPeriodUsage{}, false
		}
		sl, ok := decodeCursorSpendLimit(nested)
		if !ok {
			return cursorPeriodUsage{}, false
		}
		out.spendLimit = &sl
	}
	return out, true
}

func decodeCursorPlanUsage(fields map[int]cursorField) (cursorPlanUsage, bool) {
	out := cursorPlanUsage{}
	if v, present, valid := cursorInt32(fields, 2); !valid {
		return cursorPlanUsage{}, false
	} else if present {
		out.includedSpend = clampInt32NonNeg(v)
	}
	if v, present, valid := cursorInt32(fields, 5); !valid {
		return cursorPlanUsage{}, false
	} else if present {
		out.limit = clampInt32NonNeg(v)
	}
	if v, present, valid := cursorDouble(fields, 14); !valid && present {
		return cursorPlanUsage{}, false
	} else if present {
		out.totalPercentUsed = floatPtr(v)
	}
	return out, true
}

func decodeCursorSpendLimit(fields map[int]cursorField) (cursorSpendLimit, bool) {
	out := cursorSpendLimit{}
	if v, present, valid := cursorInt32(fields, 6); !valid {
		return cursorSpendLimit{}, false
	} else if present {
		out.individualUsed = clampInt32NonNeg(v)
	}
	if v, present, valid := cursorInt32(fields, 5); !valid {
		return cursorSpendLimit{}, false
	} else if present {
		cp := clampInt32NonNeg(v)
		out.individualLimit = &cp
	}
	if v, present, valid := cursorInt32(fields, 2); !valid {
		return cursorSpendLimit{}, false
	} else if present {
		cp := clampInt32NonNeg(v)
		out.pooledLimit = &cp
	}
	if s, present := cursorString(fields, 8); present {
		out.limitType = s
	}
	return out, true
}

func decodeCursorHardLimit(body []byte) (cursorHardLimit, bool) {
	fields, ok := decodeCursorFields(body)
	if !ok {
		return cursorHardLimit{}, false
	}
	out := cursorHardLimit{}
	if v, present, valid := cursorInt32(fields, 1); !valid {
		return cursorHardLimit{}, false
	} else if present {
		out.hardLimit = v
	}
	if v, present, valid := cursorBool(fields, 2); !valid {
		return cursorHardLimit{}, false
	} else if present {
		out.noUsageBasedAllowed = v
	}
	return out, true
}

func decodeCursorPlanInfo(body []byte) (cursorPlanInfo, bool) {
	fields, ok := decodeCursorFields(body)
	if !ok {
		return cursorPlanInfo{}, false
	}
	nested, present := cursorNested(fields, 1)
	if !present {
		return cursorPlanInfo{}, true
	}
	if nested == nil {
		return cursorPlanInfo{}, false
	}
	out := cursorPlanInfo{}
	if s, ok := cursorString(nested, 1); ok {
		out.planName = s
	}
	if v, present, valid := cursorInt64(nested, 4); !valid && present {
		return cursorPlanInfo{}, false
	} else if present {
		out.billingCycleEnd = v
	}
	return out, true
}

func decodeCursorMe(body []byte) (cursorMe, bool) {
	fields, ok := decodeCursorFields(body)
	if !ok {
		return cursorMe{}, false
	}
	out := cursorMe{}
	if v, present, valid := cursorInt32(fields, 2); !valid {
		return cursorMe{}, false
	} else if present {
		out.userID = v
	}
	if v, present, valid := cursorInt32(fields, 7); !valid {
		return cursorMe{}, false
	} else if present {
		out.teamID = v
	}
	if v, present, valid := cursorBool(fields, 9); !valid {
		return cursorMe{}, false
	} else if present {
		out.isEnterpriseUser = v
	}
	return out, true
}

func clampInt32NonNeg(v int32) int32 {
	if v < 0 {
		return 0
	}
	return v
}

type cursorField struct {
	number   int
	wireType int
	value    uint64
	bytes    []byte
}

func decodeCursorFields(b []byte) (map[int]cursorField, bool) {
	fields := map[int]cursorField{}
	offset := 0
	for offset < len(b) {
		f, next, ok := readCursorField(b, offset)
		if !ok {
			return nil, false
		}
		fields[f.number] = f
		offset = next
	}
	return fields, true
}

func readCursorField(b []byte, offset int) (cursorField, int, bool) {
	tag, next, ok := readCursorUvarint(b, offset)
	if !ok {
		return cursorField{}, offset, false
	}
	fieldNumber := int(tag >> 3)
	wireType := int(tag & 0x7)
	if fieldNumber == 0 {
		return cursorField{}, offset, false
	}
	switch wireType {
	case cursorWireVarint:
		v, after, ok := readCursorUvarint(b, next)
		if !ok {
			return cursorField{}, offset, false
		}
		return cursorField{fieldNumber, cursorWireVarint, v, nil}, after, true
	case cursorWireFixed64:
		if next+8 > len(b) {
			return cursorField{}, offset, false
		}
		return cursorField{fieldNumber, cursorWireFixed64, 0, b[next : next+8]}, next + 8, true
	case cursorWireFixed32:
		if next+4 > len(b) {
			return cursorField{}, offset, false
		}
		return cursorField{fieldNumber, cursorWireFixed32, 0, b[next : next+4]}, next + 4, true
	case cursorWireLen:
		length, bodyStart, ok := readCursorUvarint(b, next)
		if !ok {
			return cursorField{}, offset, false
		}
		if length > uint64(len(b)-bodyStart) {
			return cursorField{}, offset, false
		}
		end := bodyStart + int(length)
		return cursorField{fieldNumber, cursorWireLen, 0, b[bodyStart:end]}, end, true
	default:
		return cursorField{}, offset, false
	}
}

func readCursorUvarint(b []byte, offset int) (uint64, int, bool) {
	if offset < 0 || offset >= len(b) {
		return 0, offset, false
	}
	value, n := binary.Uvarint(b[offset:])
	if n <= 0 {
		return 0, offset, false
	}
	return value, offset + n, true
}

func cursorInt32(fields map[int]cursorField, num int) (int32, bool, bool) {
	f, ok := fields[num]
	if !ok {
		return 0, false, true
	}
	if f.wireType != cursorWireVarint {
		return 0, true, false
	}
	return int32(f.value), true, true
}

func cursorInt64(fields map[int]cursorField, num int) (int64, bool, bool) {
	f, ok := fields[num]
	if !ok {
		return 0, false, true
	}
	if f.wireType != cursorWireVarint {
		return 0, true, false
	}
	return int64(f.value), true, true
}

func cursorBool(fields map[int]cursorField, num int) (bool, bool, bool) {
	f, ok := fields[num]
	if !ok {
		return false, false, true
	}
	if f.wireType != cursorWireVarint {
		return false, true, false
	}
	return f.value != 0, true, true
}

func cursorDouble(fields map[int]cursorField, num int) (float64, bool, bool) {
	f, ok := fields[num]
	if !ok {
		return 0, false, true
	}
	if f.wireType != cursorWireFixed64 || len(f.bytes) != 8 {
		return 0, true, false
	}
	n := math.Float64frombits(binary.LittleEndian.Uint64(f.bytes))
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, true, false
	}
	return n, true, true
}

func cursorString(fields map[int]cursorField, num int) (string, bool) {
	f, ok := fields[num]
	if !ok || f.wireType != cursorWireLen {
		return "", false
	}
	return string(f.bytes), true
}

func cursorNested(fields map[int]cursorField, num int) (map[int]cursorField, bool) {
	f, ok := fields[num]
	if !ok {
		return nil, false
	}
	if f.wireType != cursorWireLen {
		return nil, true
	}
	nested, ok := decodeCursorFields(f.bytes)
	if !ok {
		return nil, true
	}
	return nested, true
}

func floatPtr(n float64) *float64 {
	v := n
	return &v
}
