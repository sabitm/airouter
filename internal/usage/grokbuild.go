package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/grokbuild"
)

const (
	grokBuildReconnect   = "Grok Build sign-in expired. Reconnect this provider."
	grokBuildConflict    = "Grok Build connection changed. Reload and retry."
	grokBuildEntitlement = "Grok Build usage is not available for this account."
	grokBuildRateLimit   = "Grok Build usage is temporarily rate limited."
	grokBuildUnavailable = "Grok Build usage is temporarily unavailable."
	grokBuildMalformed   = "Grok Build usage response was not valid."
	grokBuildNoData      = "No billing data available."

	grokBuildPeriodWeekly  = "USAGE_PERIOD_TYPE_WEEKLY"
	grokBuildPeriodMonthly = "USAGE_PERIOD_TYPE_MONTHLY"
)

type grokBuildIdentity struct {
	id         int64
	generation string
	account    string
}

func (a grokBuildIdentity) same(b grokBuildIdentity) bool {
	return a.id == b.id && a.generation == b.generation && a.account == b.account
}

type grokBuildInflightKey struct {
	id         int64
	generation string
	account    string
}

type grokBuildCacheEntry struct {
	report    *Report
	expiresAt time.Time
	identity  grokBuildIdentity
}

type centState int

const (
	centMissing centState = iota
	centNull
	centValue
	centInvalid
)

type grokBuildParsed struct {
	plan    string
	quotas  []Quota
	details []UsageDetail
}

type grokBuildParseKind int

const (
	grokBuildParseOK grokBuildParseKind = iota
	grokBuildParseEmpty
	grokBuildParseMalformed
)

// GrokBuildUsage reports a row the Usage tab may read. Protocol, OAuth bearer,
// the GrokBuildAuth marker, a compatible preset, the fixed base, and credential
// checks are all required. A preset-only row is not enough.
func GrokBuildUsage(p *domain.Provider) bool {
	if p == nil || p.Protocol != domain.ProtocolGrokBuild {
		return false
	}
	if p.Method() != domain.AuthOAuth || p.Auth() != domain.AuthBearer {
		return false
	}
	if strings.TrimSpace(p.APIKey) != "" || !grokbuild.IsFixedBase(p.BaseURL) {
		return false
	}
	c := p.OAuthCreds
	if c == nil || !c.GrokBuildAuth {
		return false
	}
	if c.Preset != "" && c.Preset != grokbuild.Preset {
		return false
	}
	return grokbuild.ValidateCredentialUse(p) == nil
}

// GrokBuildConflictReport is the card shown when the stored connection changed
// after a usage read started. It does not include the previous report.
func GrokBuildConflictReport() *Report {
	return &Report{Message: grokBuildConflict}
}

// GrokBuildStillCurrent reports whether rep may be rendered for fresh. A
// stamped report is compared with the row loaded after the fetch. That
// comparison does not refresh a token. An unstamped hard error falls back to
// the row the request started with. A save that lands after this check can
// still be missed.
func GrokBuildStillCurrent(start, fresh *domain.Provider, rep *Report) bool {
	if start == nil || fresh == nil || start.ID != fresh.ID || !GrokBuildUsage(fresh) {
		return false
	}
	if rep != nil && rep.grokBuildGeneration != "" {
		idn := grokBuildIdentityOf(fresh)
		return rep.grokBuildID == fresh.ID && rep.grokBuildGeneration == idn.generation && rep.grokBuildAccount == idn.account
	}
	return grokBuildIdentityOf(start).same(grokBuildIdentityOf(fresh))
}

func (s *Service) fetchGrokBuildWith(ctx context.Context, p *domain.Provider, opts FetchOpts) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snap := cloneUsageProvider(p)
	if snap.ID == 0 {
		return s.fetchGrokBuild(ctx, snap)
	}
	// The caller passes the stored row. A cache hit uses that identity and does
	// not resolve or refresh. opts.Force skips the stored report only.
	idn := grokBuildIdentityOf(snap)
	s.mu.Lock()
	if !opts.Force {
		if rep, ok := s.grokBuildCacheHit(idn); ok {
			s.mu.Unlock()
			return rep, nil
		}
	}
	key := grokBuildInflightKey{id: idn.id, generation: idn.generation, account: idn.account}
	if f, ok := s.grokBuildInflight[key]; ok {
		s.mu.Unlock()
		return waitInflight(ctx, f)
	}
	f := &inflight{done: make(chan struct{})}
	s.grokBuildInflight[key] = f
	epoch := s.beginGrokBuildFetch(idn)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if cur := s.grokBuildInflight[key]; cur == f {
			delete(s.grokBuildInflight, key)
		}
		close(f.done)
		s.mu.Unlock()
	}()

	report, err := s.fetchGrokBuild(ctx, snap)
	if ctxErr := ctx.Err(); ctxErr != nil {
		f.report, f.err = nil, ctxErr
		return nil, ctxErr
	}
	f.report, f.err = report, err
	if err == nil && grokBuildCacheable(report) {
		// Cache the resolved connection, not the fingerprint this fetch started
		// with. A legacy refresh mints that id inside ResolveCreds; the stored
		// row after that CAS uses the minted id.
		resolved := grokBuildIdentity{id: snap.ID, generation: report.grokBuildGeneration, account: report.grokBuildAccount}
		s.storeGrokBuildReport(idn, resolved, epoch, report)
	}
	return report, err
}

// beginGrokBuildFetch records the caller identity that may write this provider.
// A different connection drops the previous report and invalidates its writer.
// Caller holds s.mu.
func (s *Service) beginGrokBuildFetch(idn grokBuildIdentity) uint64 {
	if prev, ok := s.grokBuildLatest[idn.id]; ok && !prev.same(idn) {
		delete(s.grokBuildCache, idn.id)
		s.grokBuildEpoch[idn.id]++
	}
	s.grokBuildLatest[idn.id] = idn
	return s.grokBuildEpoch[idn.id]
}

func (s *Service) grokBuildCacheHit(idn grokBuildIdentity) (*Report, bool) {
	e, ok := s.grokBuildCache[idn.id]
	if !ok || !e.identity.same(idn) || !s.now().Before(e.expiresAt) {
		return nil, false
	}
	return e.report, true
}

func (s *Service) storeGrokBuildReport(caller, resolved grokBuildIdentity, epoch uint64, report *Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grokBuildEpoch[caller.id] != epoch || !s.grokBuildLatest[caller.id].same(caller) {
		return
	}
	s.grokBuildCache[caller.id] = grokBuildCacheEntry{
		report:    report,
		expiresAt: s.now().Add(cacheTTL),
		identity:  resolved,
	}
}

func grokBuildCacheable(r *Report) bool {
	if r == nil || r.Message != "" || r.grokBuildGeneration == "" {
		return false
	}
	return len(r.Quotas) > 0 || len(r.Details) > 0 || r.Plan != ""
}

func (s *Service) fetchGrokBuild(ctx context.Context, p *domain.Provider) (*Report, error) {
	s.logger.Debug("usage_fetch",
		"event", "usage_fetch",
		"provider_id", p.ID,
		"protocol", p.Protocol,
	)
	// Force on the usage cache does not reach this function as an OAuth force.
	creds, err := s.resolveGrokBuildCreds(ctx, p, false)
	if rep, ret, stop := s.classifyGrokBuildResolve(err, creds); stop {
		return rep, ret
	}
	if !grokBuildResolverAccepted(p.OAuthCreds, creds) || !GrokBuildUsage(providerWithCreds(p, creds)) {
		return s.grokBuildReport(grokBuildConflict, nil, nil), nil
	}
	status, body, err := s.sendGrokBuildBilling(ctx, creds)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return s.stampGrokBuild(s.grokBuildReport(grokBuildUnavailable, nil, nil), p.ID, creds), nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Retry once with the snapshot that was rejected. Do not send that
		// token again, and do not adopt another connection.
		next, nerr := s.resolveGrokBuildCreds(ctx, providerWithCreds(p, creds), true)
		if rep, ret, stop := s.classifyGrokBuildResolve(nerr, next); stop {
			return rep, ret
		}
		if !grokBuildResolverAccepted(creds, next) || !next.GrokBuildAuth {
			return s.grokBuildReport(grokBuildConflict, nil, nil), nil
		}
		if strings.TrimSpace(next.AccessToken) == "" || next.AccessToken == creds.AccessToken {
			return s.stampGrokBuild(s.grokBuildReport(grokBuildReconnect, nil, nil), p.ID, creds), nil
		}
		if !GrokBuildUsage(providerWithCreds(p, next)) {
			return s.stampGrokBuild(s.grokBuildReport(grokBuildUnavailable, nil, nil), p.ID, creds), nil
		}
		creds = next
		status, body, err = s.sendGrokBuildBilling(ctx, creds)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return s.stampGrokBuild(s.grokBuildReport(grokBuildUnavailable, nil, nil), p.ID, creds), nil
		}
	}
	return s.stampGrokBuild(s.interpretGrokBuild(status, body), p.ID, creds), nil
}

func (s *Service) resolveGrokBuildCreds(ctx context.Context, p *domain.Provider, force bool) (*domain.OAuthCreds, error) {
	r, ok := s.oauth.(credsResolver)
	if !ok || r == nil {
		return nil, ErrNoToken
	}
	creds, err := r.ResolveCreds(ctx, p, force)
	if err != nil {
		return nil, err
	}
	return creds, nil
}

func (s *Service) classifyGrokBuildResolve(err error, creds *domain.OAuthCreds) (*Report, error, bool) {
	if err != nil {
		switch {
		case errors.Is(err, ErrNoToken):
			return nil, ErrNoToken, true
		case oauth.IsInvalidGrant(err):
			return s.grokBuildReport(grokBuildReconnect, nil, nil), nil, true
		case oauth.IsCredentialConflict(err):
			// The resolver already refused the other connection. Do not read a
			// fresh row or retry through it.
			return s.grokBuildReport(grokBuildConflict, nil, nil), nil, true
		default:
			return s.grokBuildReport(grokBuildUnavailable, nil, nil), nil, true
		}
	}
	if creds == nil || !creds.GrokBuildAuth || strings.TrimSpace(creds.AccessToken) == "" {
		return nil, ErrNoToken, true
	}
	return nil, nil, false
}

func (s *Service) sendGrokBuildBilling(ctx context.Context, creds *domain.OAuthCreds) (int, []byte, error) {
	token, userID := "", ""
	if creds != nil {
		token = creds.AccessToken
		userID = creds.UserID
	}
	start := s.now()
	status, body, err := grokbuild.FetchBilling(ctx, token, userID)
	host := "cli-chat-proxy.grok.com"
	if err != nil && status != http.StatusUpgradeRequired {
		s.logger.Debug("usage_transport_failed",
			"event", "usage_transport_failed",
			"method", http.MethodGet,
			"host", host,
			"duration_ms", s.now().Sub(start).Milliseconds(),
		)
		return 0, nil, err
	}
	s.logger.Debug("usage_response",
		"event", "usage_response",
		"method", http.MethodGet,
		"host", host,
		"status", status,
		"duration_ms", s.now().Sub(start).Milliseconds(),
		"size", len(body),
	)
	if status == http.StatusUpgradeRequired {
		return status, nil, nil
	}
	return status, body, nil
}

func (s *Service) interpretGrokBuild(status int, body []byte) *Report {
	switch status {
	case http.StatusUnauthorized:
		return s.grokBuildReport(grokBuildReconnect, nil, nil)
	case http.StatusForbidden, http.StatusPaymentRequired:
		return s.grokBuildReport(grokBuildEntitlement, nil, nil)
	case http.StatusTooManyRequests:
		return s.grokBuildReport(grokBuildRateLimit, nil, nil)
	case http.StatusUpgradeRequired:
		return s.grokBuildReport(grokbuild.VersionMismatchError().Error(), nil, nil)
	}
	if status < 200 || status >= 300 {
		return s.grokBuildReport(grokBuildUnavailable, nil, nil)
	}
	parsed, kind := parseGrokBuildBilling(body)
	switch kind {
	case grokBuildParseMalformed:
		return s.grokBuildReport(grokBuildMalformed, nil, nil)
	case grokBuildParseEmpty:
		return s.grokBuildReport(grokBuildNoData, nil, nil)
	default:
		return &Report{
			Plan:      parsed.plan,
			Quotas:    parsed.quotas,
			Details:   parsed.details,
			FetchedAt: s.now(),
		}
	}
}

func (s *Service) grokBuildReport(msg string, quotas []Quota, details []UsageDetail) *Report {
	return &Report{Message: msg, Quotas: quotas, Details: details, FetchedAt: s.now()}
}

func (s *Service) stampGrokBuild(rep *Report, id int64, creds *domain.OAuthCreds) *Report {
	if rep == nil {
		return nil
	}
	idn := grokBuildIdentityFrom(id, creds)
	rep.grokBuildID = idn.id
	rep.grokBuildGeneration = idn.generation
	rep.grokBuildAccount = idn.account
	return rep
}

// grokBuildResolverAccepted reports whether resolved belongs to the exact
// snapshot passed to a successful ResolveCreds call. It does not read the
// store and must not be used on a row loaded after a conflict.
//
// sameGrokBuildUsageConn stays strict: an established connection id must
// match, and an established id that is missing is a conflict. The only added
// case is a blank id becoming nonempty. That transition is trusted only
// because oauth.Service.doRefreshGrokBuild checks the original basis against
// the live row and writes the minted id inside the generation-checked CAS.
// For that upgrade, a successful ResolveCreds result is the CAS snapshot, not
// another connection. A blank account may be filled. A nonblank account
// mismatch stays a conflict.
func grokBuildResolverAccepted(requested, resolved *domain.OAuthCreds) bool {
	if sameGrokBuildUsageConn(requested, resolved) {
		return true
	}
	return grokBuildLegacyMintFromResolver(requested, resolved)
}

// grokBuildLegacyMintFromResolver is the one-way blank connection upgrade from
// doRefreshGrokBuild. It is not a reconnect rule.
func grokBuildLegacyMintFromResolver(requested, resolved *domain.OAuthCreds) bool {
	if requested == nil || resolved == nil || !resolved.GrokBuildAuth {
		return false
	}
	if requested.Preset != "" && requested.Preset != grokbuild.Preset {
		return false
	}
	if resolved.Preset != "" && resolved.Preset != grokbuild.Preset {
		return false
	}
	if requested.GrokBuildConnection != "" || resolved.GrokBuildConnection == "" {
		return false
	}
	if requested.UserID != "" && resolved.UserID != "" && requested.UserID != resolved.UserID {
		return false
	}
	return true
}

func sameGrokBuildUsageConn(requested, resolved *domain.OAuthCreds) bool {
	if requested == nil || resolved == nil || !resolved.GrokBuildAuth {
		return false
	}
	if requested.Preset != "" && requested.Preset != grokbuild.Preset {
		return false
	}
	if resolved.Preset != "" && resolved.Preset != grokbuild.Preset {
		return false
	}
	if requested.GrokBuildConnection != "" || resolved.GrokBuildConnection != "" {
		if requested.GrokBuildConnection == "" || requested.GrokBuildConnection != resolved.GrokBuildConnection {
			return false
		}
	}
	if requested.UserID != "" && resolved.UserID != "" && requested.UserID != resolved.UserID {
		return false
	}
	return true
}

func grokBuildIdentityOf(p *domain.Provider) grokBuildIdentity {
	if p == nil {
		return grokBuildIdentity{}
	}
	return grokBuildIdentityFrom(p.ID, p.OAuthCreds)
}

func grokBuildIdentityFrom(id int64, c *domain.OAuthCreds) grokBuildIdentity {
	return grokBuildIdentity{
		id:         id,
		generation: grokBuildGeneration(c),
		account:    grokBuildAccount(c),
	}
}

func grokBuildAccount(c *domain.OAuthCreds) string {
	if c == nil {
		return ""
	}
	return grokbuild.NormalizeIdentity(c.UserID)
}

// grokBuildGeneration is the connection id when the row has one. A legacy row
// uses a SHA-256 fingerprint of the credential snapshot. The returned key does
// not contain the raw secret. A legacy refresh changes the fingerprint, so
// that row does not reuse the cache across rotation.
func grokBuildGeneration(c *domain.OAuthCreds) string {
	if c == nil {
		return ""
	}
	if id := strings.TrimSpace(c.GrokBuildConnection); id != "" {
		return "c:" + id
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		c.AccessToken,
		c.RefreshToken,
		strconv.FormatInt(c.ExpiresAt, 10),
		c.UserID,
		c.AccountID,
		c.ClientID,
	}, "\x00")))
	return "f:" + hex.EncodeToString(sum[:])
}

func parseGrokBuildBilling(body []byte) (grokBuildParsed, grokBuildParseKind) {
	if len(bytes.TrimSpace(body)) == 0 {
		return grokBuildParsed{}, grokBuildParseMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return grokBuildParsed{}, grokBuildParseMalformed
	}
	// Decoder.More reports another array or object element. After a top-level
	// value it treats a stray ']' or '}' as the end, so it is not an EOF check.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return grokBuildParsed{}, grokBuildParseMalformed
	}
	plan, tierInvalid := billingTier(root)
	raw, present := root["config"]
	if !present || raw == nil {
		if tierInvalid {
			return grokBuildParsed{}, grokBuildParseMalformed
		}
		if plan != "" {
			return grokBuildParsed{plan: plan}, grokBuildParseOK
		}
		return grokBuildParsed{}, grokBuildParseEmpty
	}
	config, ok := raw.(map[string]any)
	if !ok {
		return grokBuildParsed{}, grokBuildParseMalformed
	}

	var parsed grokBuildParsed
	sawValid := false
	sawInvalid := tierInvalid
	if plan != "" {
		parsed.plan = plan
		sawValid = true
	}

	pct, ps := parsePercentField(config, "creditUsagePercent")
	window, periodReset, periodInvalid := periodWindow(config)
	if periodInvalid {
		sawInvalid = true
		window, periodReset = "", nil
	}
	switch ps {
	case centValue:
		parsed.quotas = append(parsed.quotas, includedQuota(pct, window, periodReset))
		sawValid = true
	case centInvalid:
		sawInvalid = true
	default:
		q, ok, invalid := legacyQuota(config)
		if invalid {
			sawInvalid = true
		}
		if ok {
			parsed.quotas = append(parsed.quotas, q)
			sawValid = true
		}
	}

	if detail, ok, invalid := prepaidDetail(config); invalid {
		sawInvalid = true
		if ok {
			parsed.details = append(parsed.details, detail)
			sawValid = true
		}
	} else if ok {
		parsed.details = append(parsed.details, detail)
		sawValid = true
	}
	if detail, ok, invalid := onDemandDetail(config, onDemandApplicable(root)); invalid {
		sawInvalid = true
		if ok {
			parsed.details = append(parsed.details, detail)
			sawValid = true
		}
	} else if ok {
		parsed.details = append(parsed.details, detail)
		sawValid = true
	}

	if sawValid {
		return parsed, grokBuildParseOK
	}
	if sawInvalid {
		return grokBuildParsed{}, grokBuildParseMalformed
	}
	return grokBuildParsed{}, grokBuildParseEmpty
}

func includedQuota(pct float64, window string, reset *time.Time) Quota {
	name := "Included"
	if window != "" {
		name = window
	}
	remaining := 100 - pct
	q := Quota{
		Name:         name,
		Used:         pct,
		Total:        100,
		Remaining:    remaining,
		RemainingPct: pctPtr(remaining),
	}
	if reset != nil {
		q.ResetAt = reset
		q.ResetTitle = reset.UTC().Format(time.RFC3339)
	}
	return q
}

func legacyQuota(config map[string]any) (Quota, bool, bool) {
	limit, ls := parseCentField(config, "monthlyLimit")
	used, us := parseCentField(config, "used")
	invalid := ls == centInvalid || us == centInvalid
	if ls != centValue || us != centValue || limit <= 0 {
		return Quota{}, false, invalid
	}
	total := float64(limit)
	spent := float64(used)
	remaining := total - spent
	if remaining < 0 {
		remaining = 0
	}
	q := Quota{
		Name:         "Monthly included",
		Used:         spent,
		Total:        total,
		Remaining:    remaining,
		RemainingPct: pctPtr(remaining / total * 100),
		Unit:         "USD cents",
	}
	if end, ok := config["billingPeriodEnd"]; ok && end != nil {
		if t := parseRFC3339(end); t != nil {
			q.ResetAt = t
			q.ResetTitle = t.UTC().Format(time.RFC3339)
		}
	}
	return q, true, invalid
}

func prepaidDetail(config map[string]any) (UsageDetail, bool, bool) {
	cents, state := parseCentField(config, "prepaidBalance")
	if state == centInvalid {
		return UsageDetail{}, false, true
	}
	if state != centValue {
		return UsageDetail{}, false, false
	}
	return UsageDetail{Label: "Prepaid balance", Value: formatUSDCents(cents)}, true, false
}

func onDemandDetail(config map[string]any, applicable bool) (UsageDetail, bool, bool) {
	cap, cs := parseCentField(config, "onDemandCap")
	used, us := parseCentField(config, "onDemandUsed")
	invalid := cs == centInvalid || us == centInvalid
	if !applicable {
		return UsageDetail{}, false, invalid
	}
	switch {
	case us == centValue && cs == centValue:
		return UsageDetail{
			Label: "On-demand",
			Value: formatUSDCents(used) + " spent of " + formatUSDCents(cap),
		}, true, invalid
	case us == centValue:
		return UsageDetail{Label: "On-demand spent", Value: formatUSDCents(used)}, true, invalid
	case cs == centValue:
		return UsageDetail{Label: "On-demand cap", Value: formatUSDCents(cap)}, true, invalid
	default:
		return UsageDetail{}, false, invalid
	}
}

func periodWindow(config map[string]any) (string, *time.Time, bool) {
	raw, ok := config["currentPeriod"]
	if !ok || raw == nil {
		return "", nil, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return "", nil, true
	}
	var name string
	switch strings.TrimSpace(asString(m["type"])) {
	case grokBuildPeriodWeekly:
		name = "Weekly"
	case grokBuildPeriodMonthly:
		name = "Monthly"
	}
	var reset *time.Time
	if end, ok := m["end"]; ok && end != nil {
		reset = parseRFC3339(end)
	}
	return name, reset, false
}

func billingTier(root map[string]any) (string, bool) {
	var vals []string
	for _, key := range []string{"subscriptionTier", "subscription_tier"} {
		raw, ok := root[key]
		if !ok || raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			return "", true
		}
		s = cleanBillingTier(s)
		if s == "" {
			continue
		}
		vals = append(vals, s)
	}
	if len(vals) == 2 && vals[0] != vals[1] {
		return "", true
	}
	if len(vals) > 0 {
		return vals[0], false
	}
	return "", false
}

func cleanBillingTier(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 128 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return ""
		}
	}
	return s
}

func onDemandApplicable(root map[string]any) bool {
	for _, key := range []string{"onDemandEnabled", "on_demand_enabled"} {
		raw, ok := root[key]
		if !ok || raw == nil {
			continue
		}
		b, ok := raw.(bool)
		if ok && !b {
			return false
		}
	}
	return true
}

func parsePercentField(m map[string]any, key string) (float64, centState) {
	raw, ok := m[key]
	if !ok {
		return 0, centMissing
	}
	if raw == nil {
		return 0, centNull
	}
	n, ok := finitePercent(raw)
	if !ok {
		return 0, centInvalid
	}
	return n, centValue
}

func finitePercent(v any) (float64, bool) {
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case json.Number:
		var err error
		n, err = t.Float64()
		if err != nil {
			return 0, false
		}
	case string:
		var err error
		n, err = strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
	case int:
		n = float64(t)
	case int64:
		n = float64(t)
	default:
		return 0, false
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
		return 0, false
	}
	return n, true
}

func parseCentField(m map[string]any, key string) (int64, centState) {
	raw, ok := m[key]
	if !ok {
		return 0, centMissing
	}
	if raw == nil {
		return 0, centNull
	}
	return parseCentValue(raw)
}

func parseCentValue(v any) (int64, centState) {
	if m, ok := v.(map[string]any); ok {
		if len(m) == 0 {
			return 0, centValue
		}
		raw, ok := m["val"]
		if !ok || raw == nil {
			return 0, centInvalid
		}
		return parseCentScalar(raw)
	}
	return parseCentScalar(v)
}

func parseCentScalar(v any) (int64, centState) {
	switch t := v.(type) {
	case float64:
		return centsFromFloat(t)
	case json.Number:
		return centsFromString(t.String())
	case string:
		return centsFromString(t)
	case int:
		return centsFromInt(int64(t))
	case int64:
		return centsFromInt(t)
	default:
		return 0, centInvalid
	}
}

func centsFromFloat(n float64) (int64, centState) {
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n >= float64(math.MaxInt64) || n != math.Trunc(n) {
		return 0, centInvalid
	}
	return int64(n), centValue
}

func centsFromString(s string) (int64, centState) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, centInvalid
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, centInvalid
	}
	return n, centValue
}

func centsFromInt(n int64) (int64, centState) {
	if n < 0 {
		return 0, centInvalid
	}
	return n, centValue
}

func parseRFC3339(v any) *time.Time {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		u := t.UTC()
		return &u
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		u := t.UTC()
		return &u
	}
	return nil
}

func formatUSDCents(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
