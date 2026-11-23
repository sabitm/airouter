package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/grokbuild"
)

func TestParseGrokBuildBillingModernWinsAndLegacyFallback(t *testing.T) {
	weeklyEnd := "2026-06-08T00:00:00Z"
	legacyEnd := "2026-07-15T00:00:00Z"
	modern := `{
		"subscription_tier":"SuperGrok Heavy",
		"config":{
			"creditUsagePercent":0,
			"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-06-01T00:00:00Z","end":"` + weeklyEnd + `"},
			"monthlyLimit":{"val":2000},
			"used":{"val":850},
			"billingPeriodEnd":"` + legacyEnd + `",
			"onDemandCap":{"val":5000},
			"onDemandUsed":{"val":0},
			"prepaidBalance":{"val":1250},
			"history":[{"period":{"type":"USAGE_PERIOD_TYPE_WEEKLY"}}],
			"productUsage":[{"product":"PRODUCT_GROK_BUILD","usagePercent":61.2}]
		}
	}`
	parsed, kind := parseGrokBuildBilling([]byte(modern))
	if kind != grokBuildParseOK || parsed.plan != "SuperGrok Heavy" || len(parsed.quotas) != 1 {
		t.Fatalf("kind=%v parsed=%+v", kind, parsed)
	}
	q := parsed.quotas[0]
	if q.Name != "Weekly" || q.Used != 0 || q.Total != 100 || q.ResetTitle != weeklyEnd {
		t.Fatalf("modern quota = %+v", q)
	}
	if strings.Contains(q.ResetTitle, "2026-07-15") || q.Unit != "" {
		t.Fatalf("legacy reset leaked into weekly quota: %+v", q)
	}
	if len(parsed.details) != 2 || parsed.details[0].Value != "$12.50" || parsed.details[1].Value != "$0.00 spent of $50.00" {
		t.Fatalf("details = %+v", parsed.details)
	}
	for _, d := range parsed.details {
		if d.Label == "Prepaid" || strings.Contains(d.Label, "quota") {
			t.Fatalf("balance rendered as a quota label: %+v", d)
		}
	}

	legacy := `{
		"config":{
			"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"` + weeklyEnd + `"},
			"monthlyLimit":{"val":2000},
			"used":{"val":850},
			"billingPeriodEnd":"` + legacyEnd + `"
		}
	}`
	parsed, kind = parseGrokBuildBilling([]byte(legacy))
	if kind != grokBuildParseOK || len(parsed.quotas) != 1 || parsed.plan != "" {
		t.Fatalf("legacy kind=%v parsed=%+v", kind, parsed)
	}
	q = parsed.quotas[0]
	if q.Name != "Monthly included" || q.Used != 850 || q.Total != 2000 || q.Unit != "USD cents" || q.ResetTitle != legacyEnd {
		t.Fatalf("legacy quota = %+v", q)
	}
	if strings.Contains(q.Name, "Weekly") || q.ResetTitle == weeklyEnd {
		t.Fatalf("legacy reset labeled as weekly: %+v", q)
	}

	nullPct := `{"config":{"creditUsagePercent":null,"monthlyLimit":{"val":100},"used":{"val":0},"billingPeriodEnd":"` + legacyEnd + `"}}`
	parsed, kind = parseGrokBuildBilling([]byte(nullPct))
	if kind != grokBuildParseOK || len(parsed.quotas) != 1 || parsed.quotas[0].Name != "Monthly included" || parsed.quotas[0].Used != 0 {
		t.Fatalf("null percent did not fall back: %+v", parsed)
	}
}

func TestParseGrokBuildBillingZeroMissingNullAndWrappers(t *testing.T) {
	body := `{
		"config":{
			"creditUsagePercent":"0",
			"prepaidBalance":{},
			"onDemandCap":{"val":"5000"},
			"onDemandUsed":0
		}
	}`
	parsed, kind := parseGrokBuildBilling([]byte(body))
	if kind != grokBuildParseOK || len(parsed.quotas) != 1 || parsed.quotas[0].Used != 0 || parsed.quotas[0].Name != "Included" {
		t.Fatalf("explicit zero = %+v kind=%v", parsed, kind)
	}
	if len(parsed.details) != 2 || parsed.details[0].Value != "$0.00" || parsed.details[1].Value != "$0.00 spent of $50.00" {
		t.Fatalf("wrapper zero details = %+v", parsed.details)
	}

	missing := `{"config":{"creditUsagePercent":12,"onDemandCap":{"val":5000}}}`
	parsed, kind = parseGrokBuildBilling([]byte(missing))
	if kind != grokBuildParseOK || len(parsed.details) != 1 || parsed.details[0].Label != "On-demand cap" {
		t.Fatalf("missing spend invented a zero: %+v", parsed.details)
	}
	for _, d := range parsed.details {
		if strings.Contains(d.Value, "spent") {
			t.Fatalf("missing spend became zero: %+v", d)
		}
	}

	nulls := `{"config":{"prepaidBalance":null,"onDemandUsed":null,"onDemandCap":null}}`
	if _, kind := parseGrokBuildBilling([]byte(nulls)); kind != grokBuildParseEmpty {
		t.Fatalf("null amounts kind = %v", kind)
	}
	absent := `{"config":{}}`
	if _, kind := parseGrokBuildBilling([]byte(absent)); kind != grokBuildParseEmpty {
		t.Fatalf("absent amounts kind = %v", kind)
	}

	plain := `{"config":{"prepaidBalance":1250,"onDemandUsed":"0","onDemandCap":{"val":0}}}`
	parsed, kind = parseGrokBuildBilling([]byte(plain))
	if kind != grokBuildParseOK || parsed.details[0].Value != "$12.50" || parsed.details[1].Value != "$0.00 spent of $0.00" {
		t.Fatalf("plain numeric = %+v", parsed.details)
	}

	hidden := `{"on_demand_enabled":false,"config":{"onDemandCap":{"val":5000},"onDemandUsed":{"val":0},"prepaidBalance":{"val":0}}}`
	parsed, kind = parseGrokBuildBilling([]byte(hidden))
	if kind != grokBuildParseOK || len(parsed.details) != 1 || parsed.details[0].Label != "Prepaid balance" {
		t.Fatalf("disabled on-demand was shown: %+v", parsed.details)
	}
}

func TestParseGrokBuildBillingMalformedNoDataAndTier(t *testing.T) {
	for _, body := range []string{"", "not-json", "[]", `{"config":[]}`, `{"config":{"creditUsagePercent":"nope"}}`, `{"config":{"prepaidBalance":{"val":"nope"}}}`} {
		if _, kind := parseGrokBuildBilling([]byte(body)); kind != grokBuildParseMalformed {
			t.Fatalf("body %s kind = %v", body, kind)
		}
	}
	for _, body := range []string{`{}`, `{"config":null}`, `{"config":{"isUnifiedBillingUser":true,"history":[]}}`} {
		parsed, kind := parseGrokBuildBilling([]byte(body))
		if kind != grokBuildParseEmpty || parsed.plan != "" || len(parsed.quotas) != 0 {
			t.Fatalf("body %s kind=%v parsed=%+v", body, kind, parsed)
		}
	}
	tierOnly := `{"subscriptionTier":"SuperGrok Heavy"}`
	parsed, kind := parseGrokBuildBilling([]byte(tierOnly))
	if kind != grokBuildParseOK || parsed.plan != "SuperGrok Heavy" || len(parsed.quotas) != 0 {
		t.Fatalf("tier only = %+v kind=%v", parsed, kind)
	}
	nested := `{"config":{"subscriptionTier":"SuperGrok Heavy","isUnifiedBillingUser":true}}`
	parsed, kind = parseGrokBuildBilling([]byte(nested))
	if kind != grokBuildParseEmpty || parsed.plan != "" {
		t.Fatalf("inferred tier = %+v kind=%v", parsed, kind)
	}
	unknown := `{"config":{"creditUsagePercent":10,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_DAILY","end":"2026-06-08T00:00:00Z"},"billingPeriodEnd":"2026-07-15T00:00:00Z"}}`
	parsed, kind = parseGrokBuildBilling([]byte(unknown))
	if kind != grokBuildParseOK || parsed.quotas[0].Name != "Included" || parsed.quotas[0].ResetTitle != "2026-06-08T00:00:00Z" {
		t.Fatalf("unknown period = %+v", parsed.quotas)
	}
	if strings.Contains(parsed.quotas[0].Name, "Weekly") || strings.Contains(parsed.quotas[0].ResetTitle, "2026-07-15") {
		t.Fatalf("unknown period used a stale weekly reset: %+v", parsed.quotas[0])
	}
}

func TestGrokBuildUsageGateLeavesGenericPathsBlocked(t *testing.T) {
	valid := grokBuildUsageProvider(4, grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	if !Supported(valid) || !GrokBuildUsage(valid) || isGrok(valid) {
		t.Fatal("valid Grok Build row was not the dedicated usage path")
	}
	invalid := []*domain.Provider{
		{Protocol: domain.ProtocolGrokBuild, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, BaseURL: grokbuild.BaseURL, OAuthCreds: &domain.OAuthCreds{Preset: grokbuild.Preset, AccessToken: "access-secret", RefreshToken: "refresh-secret"}},
		{Protocol: domain.ProtocolGrokBuild, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, BaseURL: "https://api.x.ai/v1", OAuthCreds: grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret")},
		{Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, BaseURL: grokbuild.BaseURL, OAuthCreds: grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret")},
	}
	mixed := grokBuildUsageProvider(5, grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	mixed.OAuthCreds.OpencodeConsoleAuth = true
	mixed.OAuthCreds.OpencodeOrgID = "org-secret"
	invalid = append(invalid, mixed)
	svc := testSvc(t, nil)
	var calls atomic.Int32
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("transport called")
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	if _, err := svc.Fetch(context.Background(), valid); err != ErrNoToken {
		t.Fatalf("missing resolver err = %v", err)
	}
	if tok, err := svc.resolveToken(context.Background(), valid, false); tok != "" || err != ErrUnsupported {
		t.Fatalf("resolveToken = %q err=%v", tok, err)
	}
	for _, p := range invalid {
		if Supported(p) || GrokBuildUsage(p) {
			t.Fatalf("invalid row supported: %+v", p)
		}
		if _, err := svc.Fetch(context.Background(), p); err != ErrUnsupported {
			t.Fatalf("Fetch err = %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("generic or invalid row called billing %d times", calls.Load())
	}
	if !Supported(grokProvider("tok")) || GrokBuildUsage(grokProvider("tok")) {
		t.Fatal("public xAI support changed")
	}
	console := consoleUsageProvider(3, "acct", "org-1", "console-access", "console-refresh")
	if !Supported(console) || GrokBuildUsage(console) || !isOpenCodeGoConsole(console) {
		t.Fatal("ordinary OpenCode usage changed")
	}
}

func TestGrokBuildFetchHeadersRetryAndSanitizedErrors(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	var urls []string
	var userIDs []string
	status := http.StatusUnauthorized
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		auths = append(auths, r.Header.Get("Authorization"))
		urls = append(urls, r.URL.String())
		userIDs = append(userIDs, r.Header.Get("X-Userid"))
		if r.Method != http.MethodGet || r.Header.Get("X-Email") != "" || r.Header.Get("X-Grok-Model-Override") != "" {
			t.Errorf("method=%s email=%q model=%q", r.Method, r.Header.Get("X-Email"), r.Header.Get("X-Grok-Model-Override"))
		}
		if strings.Contains(r.Header.Get("Authorization"), "refresh-secret") {
			t.Errorf("refresh token sent: %v", r.Header)
		}
		code := status
		if len(auths) > 1 {
			code = http.StatusOK
		}
		body := `{"config":{"creditUsagePercent":42.5,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-06-08T00:00:00Z"}}}`
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	stub := &grokCredsStub{first: grokBuildUsageCreds("conn-1", "user-1", "resolved-secret", "refresh-secret"), next: grokBuildUsageCreds("conn-1", "user-1", "rotated-secret", "refresh-2")}
	p := grokBuildUsageProvider(8, grokBuildUsageCreds("conn-1", "user-1", "saved-secret", "refresh-secret"))
	generic := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("generic client called %s", r.URL)
		return nil, errors.New("generic client")
	})}
	svc := newService(stub, nil, generic)
	rep, err := svc.FetchWith(context.Background(), p, FetchOpts{Force: true})
	if err != nil || rep == nil || len(rep.Quotas) != 1 || rep.Quotas[0].Used != 42.5 {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "Bearer resolved-secret" || auths[1] != "Bearer rotated-secret" {
		t.Fatalf("auths = %v", auths)
	}
	if len(urls) != 2 || urls[0] != grokbuild.BillingURL || urls[1] != grokbuild.BillingURL {
		t.Fatalf("urls = %v", urls)
	}
	if userIDs[0] != "user-1" || !equalBools(stub.forces, []bool{false, true}) || stub.seen[1] != "resolved-secret" {
		t.Fatalf("forces=%v seen=%v users=%v", stub.forces, stub.seen, userIDs)
	}
}

func TestGrokBuildRetryStopsAfterConflictInvalidGrantAndSameToken(t *testing.T) {
	var hits atomic.Int32
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("secret-body")), Request: r}, nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	p := grokBuildUsageProvider(8, grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	for _, tc := range []struct {
		name string
		err  error
		next *domain.OAuthCreds
		want string
	}{
		{"conflict", oauth.ErrCredentialConflict, nil, grokBuildConflict},
		{"invalid grant", oauth.ErrInvalidGrant, nil, grokBuildReconnect},
		{"same token", nil, grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret"), grokBuildReconnect},
		{"other connection", nil, grokBuildUsageCreds("conn-2", "user-2", "other-secret", "other-refresh"), grokBuildConflict},
		{"missing connection", nil, grokBuildUsageCreds("", "user-1", "other-secret", "other-refresh"), grokBuildConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			stub := &grokCredsStub{forceErr: tc.err, next: tc.next}
			svc := newService(stub, nil, rejectUsageClient(t))
			rep, err := svc.Fetch(context.Background(), p)
			if err != nil || rep == nil || rep.Message != tc.want || hits.Load() != before+1 {
				t.Fatalf("err=%v msg=%q hits=%d", err, repMessage(rep), hits.Load()-before)
			}
			if strings.Contains(rep.Message, "secret-body") || strings.Contains(rep.Message, "access-secret") {
				t.Fatalf("message leaked: %s", rep.Message)
			}
			if len(stub.forces) != 2 || stub.forces[0] || !stub.forces[1] {
				t.Fatalf("forces = %v", stub.forces)
			}
		})
	}
}

func TestGrokBuildStatusMessagesAreNotCached(t *testing.T) {
	var hits atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		body := "secret-body"
		return &http.Response{StatusCode: int(status.Load()), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	p := grokBuildUsageProvider(3, grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	stub := &grokCredsStub{}
	svc := newService(stub, nil, rejectUsageClient(t))
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || rep.Message != grokBuildRateLimit || hits.Load() != 1 {
		t.Fatalf("rate err=%v msg=%q hits=%d", err, repMessage(rep), hits.Load())
	}
	if _, err := svc.Fetch(context.Background(), p); err != nil || hits.Load() != 2 {
		t.Fatalf("rate limit was cached, hits=%d err=%v", hits.Load(), err)
	}

	status.Store(http.StatusForbidden)
	stub.next = grokBuildUsageCreds("conn-1", "user-1", "rotated-secret", "refresh-2")
	before := hits.Load()
	rep, err = svc.Fetch(context.Background(), p)
	if err != nil || rep.Message != grokBuildEntitlement || hits.Load() != before+2 {
		t.Fatalf("entitlement err=%v msg=%q hits=%d", err, repMessage(rep), hits.Load()-before)
	}
	status.Store(http.StatusUpgradeRequired)
	var buf bytes.Buffer
	svc.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rep, err = svc.Fetch(context.Background(), p)
	if err != nil || !strings.Contains(rep.Message, "HTTP 426") || strings.Contains(rep.Message, "secret-body") || strings.Contains(buf.String(), "secret-body") || strings.Contains(buf.String(), "access-secret") {
		t.Fatalf("426 err=%v msg=%q log=%s", err, repMessage(rep), buf.String())
	}
}

func TestGrokBuildTransportFailureDoesNotLeak(t *testing.T) {
	const location = "https://evil.example/private-upstream-detail"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for " + r.Header.Get("Authorization") + " via " + location)
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	p := grokBuildUsageProvider(3, grokBuildUsageCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	svc := newService(&grokCredsStub{}, logger, rejectUsageClient(t))
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || rep.Message != grokBuildUnavailable {
		t.Fatalf("err=%v msg=%q", err, repMessage(rep))
	}
	logged := buf.String()
	for _, secret := range []string{"access-secret", "refresh-secret", "secret@example.com", "evil.example", "private-upstream-detail", "user-1"} {
		if strings.Contains(rep.Message, secret) || strings.Contains(logged, secret) {
			t.Fatalf("secret %q leaked: msg=%q log=%s", secret, rep.Message, logged)
		}
	}
	if !strings.Contains(logged, "usage_transport_failed") {
		t.Fatalf("missing transport log: %s", logged)
	}
}

func TestGrokBuildCacheForceTTLAndReconnect(t *testing.T) {
	var hits atomic.Int32
	var pct atomic.Int32
	pct.Store(10)
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		body := `{"config":{"creditUsagePercent":` + itoa(int(pct.Load())) + `}}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stub := &grokCredsStub{}
	svc := newService(stub, nil, rejectUsageClient(t))
	svc.now = func() time.Time { return now }
	a := grokBuildUsageProvider(9, grokBuildUsageCreds("conn-a", "user-1", "token-a", "refresh-a"))
	rep, err := svc.Fetch(context.Background(), a)
	if err != nil || rep.Quotas[0].Used != 10 || hits.Load() != 1 {
		t.Fatalf("fill err=%v rep=%+v hits=%d", err, rep, hits.Load())
	}
	pct.Store(40)
	rep, err = svc.Fetch(context.Background(), a)
	if err != nil || rep.Quotas[0].Used != 10 || hits.Load() != 1 {
		t.Fatalf("cache miss err=%v used=%v hits=%d", err, quotaUsed(rep), hits.Load())
	}
	rep, err = svc.FetchWith(context.Background(), a, FetchOpts{Force: true})
	if err != nil || rep.Quotas[0].Used != 40 || hits.Load() != 2 {
		t.Fatalf("force err=%v used=%v hits=%d", err, quotaUsed(rep), hits.Load())
	}
	for _, force := range stub.forces {
		if force {
			t.Fatal("usage Force forced OAuth refresh")
		}
	}
	now = now.Add(59 * time.Second)
	if _, err := svc.Fetch(context.Background(), a); err != nil || hits.Load() != 2 {
		t.Fatalf("ttl early miss hits=%d err=%v", hits.Load(), err)
	}
	now = now.Add(time.Second)
	pct.Store(55)
	rep, err = svc.Fetch(context.Background(), a)
	if err != nil || rep.Quotas[0].Used != 55 || hits.Load() != 3 {
		t.Fatalf("ttl expiry err=%v used=%v hits=%d", err, quotaUsed(rep), hits.Load())
	}

	sameAccount := grokBuildUsageProvider(9, grokBuildUsageCreds("conn-b", "user-1", "token-b", "refresh-b"))
	pct.Store(80)
	rep, err = svc.Fetch(context.Background(), sameAccount)
	if err != nil || rep.Quotas[0].Used != 80 || hits.Load() != 4 {
		t.Fatalf("same-account reconnect reused cache err=%v used=%v hits=%d", err, quotaUsed(rep), hits.Load())
	}
	changedAccount := grokBuildUsageProvider(9, grokBuildUsageCreds("conn-b", "user-2", "token-c", "refresh-c"))
	pct.Store(15)
	rep, err = svc.Fetch(context.Background(), changedAccount)
	if err != nil || rep.Quotas[0].Used != 15 || hits.Load() != 5 {
		t.Fatalf("changed account reused cache err=%v used=%v hits=%d", err, quotaUsed(rep), hits.Load())
	}
	pct.Store(33)
	rep, err = svc.Fetch(context.Background(), sameAccount)
	if err != nil || rep.Quotas[0].Used != 33 || hits.Load() != 6 {
		t.Fatalf("replaced slot served another account err=%v used=%v hits=%d", err, quotaUsed(rep), hits.Load())
	}
}

func TestGrokBuildLegacyFingerprintDoesNotShareCache(t *testing.T) {
	var hits atomic.Int32
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"config":{"creditUsagePercent":10}}`)), Request: r}, nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	svc := newService(&grokCredsStub{}, nil, rejectUsageClient(t))
	legacy := grokBuildUsageCreds("", "user-1", "token-a", "refresh-a")
	p := grokBuildUsageProvider(2, legacy)
	if _, err := svc.Fetch(context.Background(), p); err != nil || hits.Load() != 1 {
		t.Fatal(err)
	}
	if _, err := svc.Fetch(context.Background(), p); err != nil || hits.Load() != 1 {
		t.Fatalf("same legacy snapshot missed cache hits=%d", hits.Load())
	}
	rotated := grokBuildUsageCreds("", "user-1", "token-b", "refresh-b")
	if _, err := svc.Fetch(context.Background(), grokBuildUsageProvider(2, rotated)); err != nil || hits.Load() != 2 {
		t.Fatalf("rotated legacy snapshot reused cache hits=%d err=%v", hits.Load(), err)
	}
}

func TestGrokBuildOldFlightDoesNotClobberNewConnection(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	var blockA atomic.Bool
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		token := r.Header.Get("Authorization")
		if token == "Bearer token-a" && blockA.Load() {
			startOnce.Do(func() { close(started) })
			<-releaseCh
			return grokBuildJSON(r, `{"config":{"creditUsagePercent":99}}`), nil
		}
		body := `{"config":{"creditUsagePercent":11}}`
		if token == "Bearer token-b" {
			body = `{"config":{"creditUsagePercent":22}}`
		}
		return grokBuildJSON(r, body), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	svc := newService(&grokCredsStub{}, nil, rejectUsageClient(t))
	a := grokBuildUsageProvider(7, grokBuildUsageCreds("conn-a", "user-a", "token-a", "refresh-a"))
	b := grokBuildUsageProvider(7, grokBuildUsageCreds("conn-b", "user-b", "token-b", "refresh-b"))
	if rep, err := svc.Fetch(context.Background(), a); err != nil || rep.Quotas[0].Used != 11 {
		t.Fatalf("fill err=%v rep=%+v", err, rep)
	}
	blockA.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.FetchWith(context.Background(), a, FetchOpts{Force: true})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked fetch did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	repB, err := svc.Fetch(ctx, b)
	if err != nil || repB.Quotas[0].Used != 22 {
		t.Fatalf("B err=%v rep=%+v", err, repB)
	}
	if hits.Load() != 3 {
		t.Fatalf("B joined A or missed, hits=%d", hits.Load())
	}
	if _, err := svc.Fetch(context.Background(), b); err != nil || hits.Load() != 3 {
		t.Fatalf("B cache miss hits=%d err=%v", hits.Load(), err)
	}
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked fetch did not finish")
	}
	repB, err = svc.Fetch(context.Background(), b)
	if err != nil || repB.Quotas[0].Used != 22 || hits.Load() != 3 {
		t.Fatalf("late A clobbered B: used=%v hits=%d err=%v", quotaUsed(repB), hits.Load(), err)
	}
	blockA.Store(false)
	repA, err := svc.Fetch(context.Background(), a)
	if err != nil || repA.Quotas[0].Used != 11 {
		t.Fatalf("A after reconnect = %+v err=%v", repA, err)
	}
	if hits.Load() != 4 {
		t.Fatalf("A did not fetch its own report, hits=%d", hits.Load())
	}
}

func TestPublicXAIUsageDoesNotUseGrokBuildBilling(t *testing.T) {
	var buildCalls atomic.Int32
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		buildCalls.Add(1)
		return nil, errors.New("grok build transport")
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/user" {
			_, _ = io.WriteString(w, `{"subscriptionTier":"SuperGrok"}`)
			return
		}
		_, _ = io.WriteString(w, `{"config":{"monthlyLimit":{"val":100},"used":{"val":25}}}`)
	}))
	t.Cleanup(up.Close)
	prevB, prevU, prevG := GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL
	GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = up.URL+"/v1/billing", up.URL+"/v1/user", up.URL+"/never"
	t.Cleanup(func() { GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = prevB, prevU, prevG })
	svc := testSvc(t, &stubResolver{token: "xai-token"})
	rep, err := svc.Fetch(context.Background(), grokProvider("xai-token"))
	if err != nil || rep == nil || len(rep.Quotas) == 0 || buildCalls.Load() != 0 {
		t.Fatalf("err=%v rep=%+v buildCalls=%d", err, rep, buildCalls.Load())
	}
}

type grokCredsStub struct {
	mu       sync.Mutex
	forces   []bool
	seen     []string
	first    *domain.OAuthCreds
	next     *domain.OAuthCreds
	forceErr error
	err      error
}

func (s *grokCredsStub) Resolve(ctx context.Context, p *domain.Provider, force bool) (string, error) {
	creds, err := s.ResolveCreds(ctx, p, force)
	if creds == nil {
		return "", err
	}
	return creds.AccessToken, err
}

func (s *grokCredsStub) ResolveCreds(_ context.Context, p *domain.Provider, force bool) (*domain.OAuthCreds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forces = append(s.forces, force)
	if p != nil && p.OAuthCreds != nil {
		s.seen = append(s.seen, p.OAuthCreds.AccessToken)
	}
	if force {
		if s.forceErr != nil {
			return nil, s.forceErr
		}
		if s.next != nil {
			cp := *s.next
			return &cp, nil
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.first != nil && !force {
		cp := *s.first
		return &cp, nil
	}
	if p == nil || p.OAuthCreds == nil {
		return nil, ErrNoToken
	}
	cp := *p.OAuthCreds
	return &cp, nil
}

func grokBuildUsageProvider(id int64, creds *domain.OAuthCreds) *domain.Provider {
	return &domain.Provider{
		ID: id, Name: "build", BaseURL: grokbuild.BaseURL,
		Protocol: domain.ProtocolGrokBuild, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: creds,
	}
}

func grokBuildUsageCreds(conn, user, access, refresh string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Preset: grokbuild.Preset, GrokBuildAuth: true, GrokBuildConnection: conn,
		UserID: user, AccessToken: access, RefreshToken: refresh,
		ExpiresAt: time.Now().Add(time.Hour).Unix(), Email: "secret@example.com",
		ClientID: grokbuild.ClientID,
	}
}

func rejectUsageClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("generic usage client called %s", r.URL)
		return nil, errors.New("generic client")
	})}
}

func grokBuildJSON(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func equalBools(got, want []bool) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func repMessage(rep *Report) string {
	if rep == nil {
		return ""
	}
	return rep.Message
}

func quotaUsed(rep *Report) float64 {
	if rep == nil || len(rep.Quotas) == 0 {
		return -1
	}
	return rep.Quotas[0].Used
}

func itoa(n int) string {
	return strconvItoa(n)
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestParseGrokBuildBillingRejectsExtraDocument(t *testing.T) {
	valid := `{"config":{"creditUsagePercent":23}}`
	for _, body := range []string{valid, valid + " ", valid + "\n\t", valid + "\r\n"} {
		parsed, kind := parseGrokBuildBilling([]byte(body))
		if kind != grokBuildParseOK || len(parsed.quotas) != 1 || parsed.quotas[0].Used != 23 {
			t.Fatalf("body %q kind=%v parsed=%+v", body, kind, parsed)
		}
	}
	number := `{"config":{"creditUsagePercent":"23.5","prepaidBalance":{"val":"0"}}}`
	parsed, kind := parseGrokBuildBilling([]byte(number + "\n"))
	if kind != grokBuildParseOK || parsed.quotas[0].Used != 23.5 || parsed.details[0].Value != "$0.00" {
		t.Fatalf("number handling changed: %+v kind=%v", parsed, kind)
	}
	for _, body := range []string{
		valid + "]",
		valid + "}",
		valid + " ]",
		valid + "\n}",
		valid + ` {"extra":true}`,
		valid + " 1",
		valid + " null",
		valid + " []",
	} {
		if _, kind := parseGrokBuildBilling([]byte(body)); kind != grokBuildParseMalformed {
			t.Fatalf("body %q kind=%v", body, kind)
		}
	}
}

func TestGrokBuildAcceptsResolverLegacyMint(t *testing.T) {
	var hits atomic.Int32
	var mu sync.Mutex
	var auths []string
	var users []string
	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		users = append(users, r.Header.Get("X-Userid"))
		mu.Unlock()
		return grokBuildJSON(r, `{"config":{"creditUsagePercent":23}}`), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	legacy := grokBuildUsageCreds("", "", "old-access", "old-refresh")
	minted := grokBuildUsageCreds("conn-minted", "user-filled", "new-access", "new-refresh")
	stub := &grokCredsStub{first: minted}
	svc := newService(stub, nil, rejectUsageClient(t))
	p := grokBuildUsageProvider(4, legacy)
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || rep == nil || rep.Message != "" || len(rep.Quotas) != 1 || rep.Quotas[0].Used != 23 {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if rep.grokBuildID != 4 || rep.grokBuildGeneration != "c:conn-minted" || rep.grokBuildAccount != "user-filled" {
		t.Fatalf("stamp id=%d gen=%s account=%s", rep.grokBuildID, rep.grokBuildGeneration, rep.grokBuildAccount)
	}
	if hits.Load() != 1 || len(auths) != 1 || auths[0] != "Bearer new-access" || users[0] != "user-filled" {
		t.Fatalf("hits=%d auths=%v users=%v", hits.Load(), auths, users)
	}
	if _, err := svc.Fetch(context.Background(), grokBuildUsageProvider(4, minted)); err != nil || hits.Load() != 1 {
		t.Fatalf("resolved identity missed cache hits=%d err=%v", hits.Load(), err)
	}
	if _, err := svc.Fetch(context.Background(), p); err != nil || hits.Load() != 2 {
		t.Fatalf("legacy fingerprint reused minted report hits=%d err=%v", hits.Load(), err)
	}

	mismatch := grokBuildUsageCreds("conn-minted", "user-2", "new-access", "new-refresh")
	stub = &grokCredsStub{first: mismatch}
	svc = newService(stub, nil, rejectUsageClient(t))
	before := hits.Load()
	rep, err = svc.Fetch(context.Background(), grokBuildUsageProvider(6, grokBuildUsageCreds("", "user-1", "old-access", "old-refresh")))
	if err != nil || rep == nil || rep.Message != grokBuildConflict || hits.Load() != before {
		t.Fatalf("principal mismatch err=%v msg=%q hits=%d", err, repMessage(rep), hits.Load()-before)
	}

	grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("secret-body")), Request: r}, nil
	}))
	stub = &grokCredsStub{next: grokBuildUsageCreds("conn-minted", "user-2", "new-access", "new-refresh")}
	svc = newService(stub, nil, rejectUsageClient(t))
	before = hits.Load()
	rep, err = svc.Fetch(context.Background(), grokBuildUsageProvider(7, grokBuildUsageCreds("", "user-1", "old-access", "old-refresh")))
	if err != nil || rep == nil || rep.Message != grokBuildConflict || hits.Load() != before+1 || strings.Contains(rep.Message, "secret-body") {
		t.Fatalf("reactive mismatch err=%v msg=%q hits=%d", err, repMessage(rep), hits.Load()-before)
	}
	if !equalBools(stub.forces, []bool{false, true}) {
		t.Fatalf("forces=%v", stub.forces)
	}
}

func TestGrokBuildMixedSnapshotDoesNotFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			var auths []string
			var mu sync.Mutex
			grokbuild.SetTestTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
				hits.Add(1)
				mu.Lock()
				auths = append(auths, r.Header.Get("Authorization"))
				mu.Unlock()
				if r.URL.String() != grokbuild.BillingURL {
					t.Errorf("url = %s", r.URL)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream-secret-body")), Request: r}, nil
			}))
			t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
			next := grokBuildUsageCreds("conn-1", "user-1", "new-access", "new-refresh")
			next.OpencodeConsoleAuth = true
			next.OpencodeOrgID = "org-secret"
			stub := &grokCredsStub{next: next}
			svc := newService(stub, nil, rejectUsageClient(t))
			p := grokBuildUsageProvider(8, grokBuildUsageCreds("conn-1", "user-1", "old-access", "old-refresh"))
			rep, err := svc.Fetch(context.Background(), p)
			if err != nil || rep == nil || rep.Message != grokBuildUnavailable || hits.Load() != 1 {
				t.Fatalf("err=%v msg=%q hits=%d", err, repMessage(rep), hits.Load())
			}
			if len(auths) != 1 || auths[0] != "Bearer old-access" || !equalBools(stub.forces, []bool{false, true}) {
				t.Fatalf("auths=%v forces=%v", auths, stub.forces)
			}
			for _, secret := range []string{"upstream-secret-body", "old-access", "new-access", "refresh-secret", "org-secret"} {
				if strings.Contains(rep.Message, secret) {
					t.Fatalf("leaked %s in %q", secret, rep.Message)
				}
			}
		})
	}
}
