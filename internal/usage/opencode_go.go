package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/opencode"
)

const (
	openCodeGoPlan          = "OpenCode Go"
	openCodeGoReconnect     = "reconnect required"
	openCodeGoConflict      = "OpenCode Console connection changed; reload and retry"
	openCodeGoTransient     = "OpenCode Go usage is temporarily unavailable."
	openCodeGoMalformed     = "OpenCode Go usage response was not valid."
	openCodeGoUnavailable   = "OpenCode Go usage is not available for this connection."
	openCodeGoLabelFallback = "the connected workspace"
)

// errOpenCodeGoUnsafe is local. The user-facing text does not include the
// helper error, which can name the rejected destination class.
var errOpenCodeGoUnsafe = errors.New("usage: opencode go target refused")

// consoleIdentity is the Usage-tab account for one saved provider. It does not
// include tokens. A refresh of the same account and workspace keeps this key,
// so the existing TTL still applies.
type consoleIdentity struct {
	id      int64
	account string
	org     string
}

type consoleInflightKey struct {
	idn   consoleIdentity
	grant [32]byte
}

type consoleCacheEntry struct {
	report    *Report
	expiresAt time.Time
	grant     [32]byte
	// success is a report with at least one usable window. A failure is reused
	// only for the same grant, so a reconnect does not inherit a dead sign-in.
	success bool
}

// isOpenCodeGoConsole reports official Go Console connections. Zen, Go API
// keys, custom or mirror URLs, and unmarked OAuth rows stay off the Usage tab.
// Archived filtering remains the caller's job.
func isOpenCodeGoConsole(p *domain.Provider) bool {
	if p == nil || p.Protocol != domain.ProtocolOpencode || p.Method() != domain.AuthOAuth {
		return false
	}
	if p.OAuthCreds == nil || !p.OAuthCreds.OpencodeConsoleAuth {
		return false
	}
	return opencode.IsOfficialGoBase(p.BaseURL)
}

func (s *Service) fetchOpenCodeGoWith(ctx context.Context, p *domain.Provider, opts FetchOpts) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snap := cloneUsageProvider(p)
	fetch := func() (*Report, error) {
		report, err := s.fetchOpenCodeGo(ctx, snap)
		// A canceled dashboard request is not an upstream outage.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return report, err
	}
	if snap.ID == 0 {
		return fetch()
	}
	idn := consoleIdentityOf(snap)
	grant := consoleGrant(snap.OAuthCreds)
	key := consoleInflightKey{idn: idn, grant: grant}

	s.mu.Lock()
	if !opts.Force {
		if rep, ok := s.consoleCacheHit(idn, grant); ok {
			s.mu.Unlock()
			return rep, nil
		}
	}
	if f, ok := s.consoleInflight[key]; ok {
		s.mu.Unlock()
		return waitInflight(ctx, f)
	}
	f := &inflight{done: make(chan struct{})}
	s.consoleInflight[key] = f
	epoch := s.beginConsoleFetch(idn, grant)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if cur := s.consoleInflight[key]; cur == f {
			delete(s.consoleInflight, key)
		}
		close(f.done)
		s.mu.Unlock()
	}()

	report, err := fetch()
	f.report, f.err = report, err
	if err == nil && report != nil {
		s.storeConsoleReport(idn, grant, epoch, report)
	}
	return report, err
}

// beginConsoleFetch records the grant that may write this identity. A different
// account or workspace evicts the previous report and invalidates its writer.
// Caller holds s.mu.
func (s *Service) beginConsoleFetch(idn consoleIdentity, grant [32]byte) uint64 {
	if prev, ok := s.consoleOwner[idn.id]; ok && (prev.account != idn.account || prev.org != idn.org) {
		delete(s.consoleCache, prev)
		s.consoleEpoch[idn.id]++
	}
	s.consoleOwner[idn.id] = idn
	s.consoleLatest[idn] = grant
	return s.consoleEpoch[idn.id]
}

func (s *Service) consoleCacheHit(idn consoleIdentity, grant [32]byte) (*Report, bool) {
	e, ok := s.consoleCache[idn]
	if !ok || !s.now().Before(e.expiresAt) {
		return nil, false
	}
	// A successful window list is workspace-scoped, so a token refresh reuses
	// it. A failure stays with the grant that produced it.
	if e.grant == grant || e.success {
		return e.report, true
	}
	return nil, false
}

func (s *Service) storeConsoleReport(idn consoleIdentity, grant [32]byte, epoch uint64, report *Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consoleEpoch[idn.id] != epoch || s.consoleLatest[idn] != grant {
		return
	}
	s.consoleCache[idn] = consoleCacheEntry{
		report:    report,
		expiresAt: s.now().Add(cacheTTL),
		grant:     grant,
		success:   len(report.Quotas) > 0,
	}
}

func (s *Service) fetchOpenCodeGo(ctx context.Context, p *domain.Provider) (*Report, error) {
	s.logger.Debug("usage_fetch",
		"event", "usage_fetch",
		"provider_id", p.ID,
		"protocol", p.Protocol,
	)
	creds, err := s.resolveOpenCodeCreds(ctx, p, false)
	if rep, ret, stop := s.classifyOpenCodeResolve(err, creds); stop {
		return rep, ret
	}
	if p.OAuthCreds != nil && !sameOpenCodeWorkspace(p.OAuthCreds, creds) {
		return s.consoleReport(openCodeGoConflict, nil), nil
	}
	res, err := s.sendOpenCodeGoUsage(ctx, p, creds)
	if errors.Is(err, errOpenCodeGoUnsafe) {
		return s.consoleReport(openCodeGoUnavailable, nil), nil
	}
	if err != nil {
		return s.consoleReport(openCodeGoTransient, nil), nil
	}
	if res.Status == http.StatusUnauthorized {
		// Pass the snapshot that was sent. A changed generation means another
		// caller already rotated that grant; forcing the original provider
		// snapshot would skip the rotation and resend the rejected token.
		next, nerr := s.resolveOpenCodeCreds(ctx, providerWithCreds(p, creds), true)
		if rep, ret, stop := s.classifyOpenCodeResolve(nerr, next); stop {
			return rep, ret
		}
		if !sameOpenCodeWorkspace(creds, next) || !next.OpencodeConsoleAuth {
			return s.consoleReport(openCodeGoConflict, nil), nil
		}
		if strings.TrimSpace(next.AccessToken) == "" || next.AccessToken == creds.AccessToken {
			return s.consoleReport(openCodeGoReconnect, nil), nil
		}
		creds = next
		res, err = s.sendOpenCodeGoUsage(ctx, p, creds)
		if errors.Is(err, errOpenCodeGoUnsafe) {
			return s.consoleReport(openCodeGoUnavailable, nil), nil
		}
		if err != nil {
			return s.consoleReport(openCodeGoTransient, nil), nil
		}
	}
	return s.interpretOpenCodeGo(res, creds), nil
}

func (s *Service) resolveOpenCodeCreds(ctx context.Context, p *domain.Provider, force bool) (*domain.OAuthCreds, error) {
	r, ok := s.oauth.(credsResolver)
	if !ok || r == nil {
		return nil, ErrNoToken
	}
	creds, err := r.ResolveCreds(ctx, p, force)
	if err != nil {
		// A mismatch can arrive with a leftover snapshot. Do not send it.
		return nil, err
	}
	return creds, nil
}

func (s *Service) classifyOpenCodeResolve(err error, creds *domain.OAuthCreds) (*Report, error, bool) {
	if err != nil {
		switch {
		case errors.Is(err, ErrNoToken):
			return nil, ErrNoToken, true
		case oauth.IsInvalidGrant(err):
			return s.consoleReport(openCodeGoReconnect, nil), nil, true
		case oauth.IsCredentialConflict(err):
			return s.consoleReport(openCodeGoConflict, nil), nil, true
		default:
			return s.consoleReport(openCodeGoTransient, nil), nil, true
		}
	}
	if !openCodeCredsReady(creds) {
		return nil, ErrNoToken, true
	}
	return nil, nil, false
}

func openCodeCredsReady(c *domain.OAuthCreds) bool {
	return c != nil && c.OpencodeConsoleAuth &&
		strings.TrimSpace(c.AccessToken) != "" &&
		strings.TrimSpace(c.RefreshToken) != "" &&
		strings.TrimSpace(c.OpencodeOrgID) != ""
}

func (s *Service) sendOpenCodeGoUsage(ctx context.Context, p *domain.Provider, creds *domain.OAuthCreds) (httpResult, error) {
	local := providerWithCreds(p, creds)
	if err := opencode.RejectUnsafeSend(local, opencode.GoUsageURL); err != nil {
		return httpResult{}, errOpenCodeGoUnsafe
	}
	return s.doOpenCodeGoUsage(ctx, creds)
}

func (s *Service) doOpenCodeGoUsage(ctx context.Context, creds *domain.OAuthCreds) (httpResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencode.GoUsageURL, nil)
	if err != nil {
		return httpResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	opencode.ApplyConsoleAuth(req.Header, creds)
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	client = opencode.ClientNoRedirect(client)
	start := time.Now()
	resp, err := client.Do(req)
	host := requestHost(opencode.GoUsageURL)
	if err != nil {
		// HTTP errors can contain raw upstream data, including Location values.
		s.logger.Debug("usage_transport_failed",
			"event", "usage_transport_failed",
			"method", http.MethodGet,
			"host", host,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return httpResult{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	s.logger.Debug("usage_response",
		"event", "usage_response",
		"method", http.MethodGet,
		"host", host,
		"status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(),
		"size", len(data),
	)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return httpResult{}, errors.New("opencode: redirect refused")
	}
	return httpResult{Status: resp.StatusCode, Body: data}, nil
}

func (s *Service) interpretOpenCodeGo(res httpResult, creds *domain.OAuthCreds) *Report {
	switch {
	case res.Status == http.StatusUnauthorized:
		return s.consoleReport(openCodeGoReconnect, nil)
	case res.Status == http.StatusForbidden && openCodeSSORequired(res.Body):
		return s.consoleReport("Sign in with SSO again to use "+openCodeGoLabel(creds)+".", nil)
	case res.Status == http.StatusForbidden:
		return s.consoleReport("OpenCode Go is not enabled for "+openCodeGoLabel(creds)+".", nil)
	case res.Status < 200 || res.Status >= 300:
		return s.consoleReport(openCodeGoTransient, nil)
	}
	quotas, msg, ok := parseOpenCodeGoUsage(res.Body)
	if !ok {
		return s.consoleReport(openCodeGoMalformed, nil)
	}
	return s.consoleReport(msg, quotas)
}

func (s *Service) consoleReport(msg string, quotas []Quota) *Report {
	return &Report{Plan: openCodeGoPlan, Message: msg, Quotas: quotas, FetchedAt: s.now()}
}

func openCodeGoLabel(c *domain.OAuthCreds) string {
	if c == nil {
		return openCodeGoLabelFallback
	}
	name := strings.TrimSpace(c.OpencodeOrgName)
	id := strings.TrimSpace(c.OpencodeOrgID)
	if name == "" || (id != "" && strings.Contains(name, id)) {
		return openCodeGoLabelFallback
	}
	return name
}

func openCodeSSORequired(body []byte) bool {
	var parsed struct {
		Tag string `json:"_tag"`
	}
	return json.Unmarshal(body, &parsed) == nil && parsed.Tag == "SsoRequired"
}

// parseOpenCodeGoUsage reads GoUsage. percent is the used share. A window is
// omitted when status, percent, or resetsAt is absent or invalid. Omission is
// not stored as zero. The payload is malformed only when every window is
// unusable. Valid windows are still shown so one bad meter does not hide the
// others.
func parseOpenCodeGoUsage(body []byte) ([]Quota, string, bool) {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || meterAbsent(envelope.Usage) {
		return nil, "", false
	}
	var windows map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Usage, &windows); err != nil {
		return nil, "", false
	}
	order := []struct {
		key  string
		name string
	}{
		{"rolling", "5-hour"},
		{"weekly", "Weekly"},
		{"monthly", "Monthly"},
	}
	var quotas []Quota
	var limited []string
	for _, spec := range order {
		q, rateLimited, ok := parseOpenCodeGoMeter(spec.name, windows[spec.key])
		if !ok {
			continue
		}
		quotas = append(quotas, q)
		if rateLimited {
			limited = append(limited, spec.name)
		}
	}
	if len(quotas) == 0 {
		return nil, "", false
	}
	msg := ""
	if len(limited) > 0 {
		msg = "Rate-limited: " + strings.Join(limited, ", ") + "."
	}
	return quotas, msg, true
}

func parseOpenCodeGoMeter(name string, raw json.RawMessage) (Quota, bool, bool) {
	if meterAbsent(raw) {
		return Quota{}, false, false
	}
	var m struct {
		Status   *string  `json:"status"`
		Percent  *float64 `json:"percent"`
		ResetsAt *string  `json:"resetsAt"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Status == nil || m.Percent == nil || m.ResetsAt == nil {
		return Quota{}, false, false
	}
	status := *m.Status
	if status != "ok" && status != "rate-limited" {
		return Quota{}, false, false
	}
	if math.IsNaN(*m.Percent) || math.IsInf(*m.Percent, 0) {
		return Quota{}, false, false
	}
	reset, ok := parseOpenCodeReset(*m.ResetsAt)
	if !ok {
		return Quota{}, false, false
	}
	// The card has no absolute unit for this payload, so leave Used and Total
	// unset and publish only the remaining percent.
	remaining := clampPct(100 - clampPct(*m.Percent))
	limited := status == "rate-limited"
	if limited {
		remaining = 0
	}
	return Quota{
		Name:         name,
		RemainingPct: pctPtr(remaining),
		ResetAt:      &reset,
	}, limited, true
}

func meterAbsent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func parseOpenCodeReset(raw string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	// RFC3339 allows a fractional second. Go's RFC3339 layout does not.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func consoleIdentityOf(p *domain.Provider) consoleIdentity {
	idn := consoleIdentity{}
	if p == nil {
		return idn
	}
	idn.id = p.ID
	if p.OAuthCreds != nil {
		idn.account = p.OAuthCreds.AccountID
		idn.org = p.OAuthCreds.OpencodeOrgID
	}
	return idn
}

func consoleGrant(c *domain.OAuthCreds) [32]byte {
	if c == nil {
		return sha256.Sum256(nil)
	}
	return sha256.Sum256([]byte(c.RefreshToken))
}

func sameOpenCodeWorkspace(a, b *domain.OAuthCreds) bool {
	if a == nil || b == nil {
		return false
	}
	return a.OpencodeOrgID == b.OpencodeOrgID && a.AccountID == b.AccountID
}

func cloneUsageProvider(p *domain.Provider) *domain.Provider {
	if p == nil {
		return nil
	}
	cp := *p
	if p.OAuthCreds != nil {
		c := *p.OAuthCreds
		cp.OAuthCreds = &c
	}
	if p.Tags != nil {
		cp.Tags = append([]string(nil), p.Tags...)
	}
	return &cp
}

func providerWithCreds(p *domain.Provider, creds *domain.OAuthCreds) *domain.Provider {
	cp := cloneUsageProvider(p)
	if creds == nil {
		cp.OAuthCreds = nil
		return cp
	}
	c := *creds
	cp.OAuthCreds = &c
	return cp
}
