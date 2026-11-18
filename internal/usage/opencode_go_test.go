package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/opencode"
)

const openCodeReset = "2030-01-01T00:00:00Z"

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func openCodeUsageBody(rolling, weekly, monthly float64, rollingStatus, reset string) string {
	if rollingStatus == "" {
		rollingStatus = "ok"
	}
	meter := func(pct float64, status string) map[string]any {
		return map[string]any{"status": status, "percent": pct, "resetsAt": reset}
	}
	// monthly is encoded first. Display order must still be rolling, weekly, monthly.
	raw, err := json.Marshal(map[string]any{
		"usage": map[string]any{
			"monthly": meter(monthly, "ok"),
			"weekly":  meter(weekly, "ok"),
			"rolling": meter(rolling, rollingStatus),
		},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func consoleUsageProvider(id int64, account, org, access, refresh string) *domain.Provider {
	return &domain.Provider{
		ID:         id,
		Name:       "go-console",
		BaseURL:    opencode.GoBaseURL,
		Protocol:   domain.ProtocolOpencode,
		AuthMethod: domain.AuthOAuth,
		AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			AccessToken:         access,
			RefreshToken:        refresh,
			ExpiresAt:           time.Now().Add(time.Hour).Unix(),
			AccountID:           account,
			OpencodeConsoleAuth: true,
			OpencodeOrgID:       org,
			OpencodeOrgName:     "Workspace",
			ClientID:            opencode.ConsoleClientID,
		},
	}
}

type passthroughCreds struct{}

func (passthroughCreds) Resolve(_ context.Context, p *domain.Provider, force bool) (string, error) {
	c, err := (passthroughCreds{}).ResolveCreds(context.Background(), p, force)
	if c == nil {
		return "", err
	}
	return c.AccessToken, err
}

func (passthroughCreds) ResolveCreds(_ context.Context, p *domain.Provider, _ bool) (*domain.OAuthCreds, error) {
	if p == nil || p.OAuthCreds == nil || strings.TrimSpace(p.OAuthCreds.AccessToken) == "" {
		return nil, ErrNoToken
	}
	c := *p.OAuthCreds
	return &c, nil
}

type credsStub struct {
	mu                 sync.Mutex
	creds              *domain.OAuthCreds
	next               *domain.OAuthCreds
	err                error
	forceErr           error
	forceN             int
	seen               []string
	returnCredsWithErr bool
}

func (s *credsStub) Resolve(ctx context.Context, p *domain.Provider, force bool) (string, error) {
	c, err := s.ResolveCreds(ctx, p, force)
	if err != nil || c == nil {
		return "", err
	}
	return c.AccessToken, nil
}

func (s *credsStub) ResolveCreds(_ context.Context, p *domain.Provider, force bool) (*domain.OAuthCreds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p != nil && p.OAuthCreds != nil {
		s.seen = append(s.seen, p.OAuthCreds.AccessToken)
	}
	if force {
		s.forceN++
		if s.forceErr != nil {
			return s.errCreds(s.forceErr)
		}
		if s.next != nil {
			c := *s.next
			s.creds = &c
		}
	} else if s.err != nil {
		return s.errCreds(s.err)
	}
	if s.creds == nil {
		return nil, ErrNoToken
	}
	c := *s.creds
	return &c, nil
}

func (s *credsStub) errCreds(err error) (*domain.OAuthCreds, error) {
	if s.returnCredsWithErr && s.creds != nil {
		c := *s.creds
		return &c, err
	}
	return nil, err
}

func (s *credsStub) seenTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.seen...)
	return out
}

func TestOpenCodeGoSupportedGating(t *testing.T) {
	console := consoleUsageProvider(1, "user-1", "org-1", "access", "refresh")
	legacy := consoleUsageProvider(2, "user-1", "org-1", "access", "refresh")
	legacy.BaseURL = opencode.LegacyGoBaseURL
	archived := consoleUsageProvider(3, "user-1", "org-1", "access", "refresh")
	archived.Archived = true
	zen := consoleUsageProvider(4, "user-1", "org-1", "access", "refresh")
	zen.BaseURL = opencode.ZenBaseURL
	apiKey := &domain.Provider{
		ID: 5, Name: "go-key", BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthAPIKey, APIKey: "go-secret",
	}
	custom := consoleUsageProvider(6, "user-1", "org-1", "access", "refresh")
	custom.BaseURL = "https://mirror.example/inference/go/v1"
	unmarked := consoleUsageProvider(7, "user-1", "org-1", "access", "refresh")
	unmarked.OAuthCreds.OpencodeConsoleAuth = false
	otherProto := consoleUsageProvider(8, "user-1", "org-1", "access", "refresh")
	otherProto.Protocol = domain.ProtocolOpenAI
	incomplete := consoleUsageProvider(9, "user-1", "org-1", "", "refresh")

	if !Supported(console) || !Supported(legacy) || !Supported(archived) || !Supported(incomplete) {
		t.Fatal("official Console Go must be supported, including archived and incomplete rows")
	}
	for _, p := range []*domain.Provider{zen, apiKey, custom, unmarked, otherProto, nil} {
		if Supported(p) {
			t.Fatalf("unsupported provider was accepted: %+v", p)
		}
	}
	svc := newService(passthroughCreds{}, nil, &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("gated provider called upstream: %s", r.URL.String())
		return jsonResp(r, 500, ""), nil
	})})
	for _, p := range []*domain.Provider{zen, apiKey, custom, unmarked} {
		if _, err := svc.Fetch(context.Background(), p); err != ErrUnsupported {
			t.Fatalf("%s err = %v, want unsupported", p.Name, err)
		}
	}
}

func TestOpenCodeGoUsageGroupingRoundingAndReset(t *testing.T) {
	var gotAuth, gotOrg, gotAPIKey, gotMethod string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != opencode.GoUsageURL {
			t.Errorf("url = %s", r.URL.String())
		}
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotOrg = r.Header.Get("x-opencode-org-id")
		gotAPIKey = r.Header.Get("x-api-key")
		body := `{"extra":true,"usage":{"monthly":{"status":"ok","percent":-4,"resetsAt":"2030-01-01T03:00:00+02:00","note":"keep"},"weekly":{"status":"rate-limited","percent":10,"resetsAt":"2030-01-01T00:00:00.5Z"},"rolling":{"status":"ok","percent":27.5,"resetsAt":"2030-01-01T00:00:00Z"}}}`
		return jsonResp(r, 200, body), nil
	})}
	svc := newService(passthroughCreds{}, nil, client)
	p := consoleUsageProvider(4, "user-1", "org-1", "access-secret", "refresh-secret")
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotAuth != "Bearer access-secret" || gotOrg != "org-1" || gotAPIKey != "" {
		t.Fatalf("method=%s auth=%q org=%q api=%q", gotMethod, gotAuth, gotOrg, gotAPIKey)
	}
	if rep.Plan != openCodeGoPlan || len(rep.Quotas) != 3 {
		t.Fatalf("plan=%q quotas=%d", rep.Plan, len(rep.Quotas))
	}
	if rep.Quotas[0].Name != "5-hour" || rep.Quotas[1].Name != "Weekly" || rep.Quotas[2].Name != "Monthly" {
		t.Fatalf("order = %s %s %s", rep.Quotas[0].Name, rep.Quotas[1].Name, rep.Quotas[2].Name)
	}
	if rep.Quotas[0].Used != 0 || rep.Quotas[0].Total != 0 || rep.Quotas[0].RemainingPct == nil || *rep.Quotas[0].RemainingPct != 72.5 {
		t.Fatalf("rolling = %+v", rep.Quotas[0])
	}
	if rep.Quotas[1].RemainingPct == nil || *rep.Quotas[1].RemainingPct != 0 || rep.Message != "Rate-limited: Weekly." {
		t.Fatalf("limited = %+v msg=%q", rep.Quotas[1], rep.Message)
	}
	if rep.Quotas[2].RemainingPct == nil || *rep.Quotas[2].RemainingPct != 100 {
		t.Fatalf("negative used was not clamped: %+v", rep.Quotas[2])
	}
	wantReset := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if rep.Quotas[0].ResetAt == nil || !rep.Quotas[0].ResetAt.Equal(wantReset) {
		t.Fatalf("rolling reset = %v", rep.Quotas[0].ResetAt)
	}
	if rep.Quotas[1].ResetAt == nil || rep.Quotas[1].ResetAt.Nanosecond() != int(time.Millisecond*500) {
		t.Fatalf("fractional reset = %v", rep.Quotas[1].ResetAt)
	}
	if rep.Quotas[2].ResetAt == nil || !rep.Quotas[2].ResetAt.Equal(time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("offset reset = %v", rep.Quotas[2].ResetAt)
	}
	if strings.Contains(rep.Message, "access-secret") || strings.Contains(rep.Message, "org-1") {
		t.Fatalf("message leaked identity: %q", rep.Message)
	}

	clamped, _, ok := parseOpenCodeGoUsage([]byte(`{"usage":{"rolling":{"status":"ok","percent":150.5,"resetsAt":"2030-01-01T00:00:00Z"},"weekly":{"status":"ok","percent":"12","resetsAt":"2030-01-01T00:00:00Z"},"monthly":{"status":"OK","percent":1,"resetsAt":"2030-01-01T00:00:00Z"}}}`))
	if !ok || len(clamped) != 1 || clamped[0].Name != "5-hour" || *clamped[0].RemainingPct != 0 {
		t.Fatalf("clamp/partial = %+v ok=%v", clamped, ok)
	}
}

func TestOpenCodeGoPartialAndMalformedPayloads(t *testing.T) {
	partial, msg, ok := parseOpenCodeGoUsage([]byte(`{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"2030-01-01T00:00:00Z"},"weekly":{"status":"ok","resetsAt":"2030-01-01T00:00:00Z"},"monthly":{"status":"ok","percent":4,"resetsAt":"1800000000"}}}`))
	if !ok || len(partial) != 1 || partial[0].Name != "5-hour" || *partial[0].RemainingPct != 100 || msg != "" {
		t.Fatalf("partial = %+v msg=%q ok=%v", partial, msg, ok)
	}
	for _, body := range []string{
		`{}`,
		`{"usage":{}}`,
		`{"usage":null}`,
		`{"usage":[]}`,
		`{"rolling":{"status":"ok","percent":1,"resetsAt":"2030-01-01T00:00:00Z"}}`,
		`{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"2030-01-01"},"weekly":{"status":"blocked","percent":1,"resetsAt":"2030-01-01T00:00:00Z"}}}`,
	} {
		quotas, _, ok := parseOpenCodeGoUsage([]byte(body))
		if ok || len(quotas) != 0 {
			t.Fatalf("body %s produced %+v", body, quotas)
		}
	}
}

func TestOpenCodeGoDoesNotFallBackToSavedToken(t *testing.T) {
	var hits atomic.Int32
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResp(r, 200, openCodeUsageBody(1, 1, 1, "ok", openCodeReset)), nil
	})}
	p := consoleUsageProvider(3, "user-1", "org-1", "snapshot-secret", "refresh-secret")
	svc := newService(&stubResolver{token: "fallback-secret"}, nil, client)
	if _, err := svc.Fetch(context.Background(), p); err != ErrNoToken {
		t.Fatalf("err = %v, want no token", err)
	}
	if hits.Load() != 0 {
		t.Fatal("fallback token was sent")
	}
	if _, err := newService(nil, nil, client).Fetch(context.Background(), p); err != ErrNoToken || hits.Load() != 0 {
		t.Fatalf("nil resolver sent or returned %v hits=%d", err, hits.Load())
	}
}

func TestOpenCodeGoAuthClasses(t *testing.T) {
	const reset = openCodeReset
	usageOK := openCodeUsageBody(10, 10, 10, "ok", reset)
	tests := []struct {
		name      string
		status    int
		body      string
		err       error
		forceErr  error
		nextOrg   string
		nextToken string
		sameToken bool
		withCreds bool
		wantHits  int32
		wantForce int
		wantMsg   string
		absent    []string
	}{
		{
			name: "invalid grant", err: oauth.ErrInvalidGrant, withCreds: true,
			wantMsg: openCodeGoReconnect, absent: []string{"access-secret", "org-1", "invalid_grant"},
		},
		{
			name: "conflict", err: oauth.ErrCredentialConflict, withCreds: true,
			wantMsg: openCodeGoConflict, absent: []string{"access-secret", "org-1"},
		},
		{
			name: "resolver transient", err: errors.New("token=access-secret upstream detail"),
			wantMsg: openCodeGoTransient, absent: []string{"access-secret", "upstream detail"},
		},
		{
			name: "terminal 401", status: http.StatusUnauthorized, body: `{"error":"invalid_grant","token":"access-secret"}`,
			sameToken: true, wantHits: 1, wantForce: 1, wantMsg: openCodeGoReconnect,
			absent: []string{"access-secret", "invalid_grant"},
		},
		{
			name: "401 then forbidden", status: http.StatusUnauthorized, body: `{"detail":"secret-body"}`,
			nextToken: "rotated-access", wantHits: 2, wantForce: 1,
			wantMsg: "OpenCode Go is not enabled for Workspace.",
			absent:  []string{"secret-body", "org-1", "rotated-access"},
		},
		{
			name: "401 refresh invalid", status: http.StatusUnauthorized, body: `{}`,
			forceErr: oauth.ErrInvalidGrant, wantHits: 1, wantForce: 1, wantMsg: openCodeGoReconnect,
		},
		{
			name: "401 refresh conflict", status: http.StatusUnauthorized, body: `{}`,
			forceErr: oauth.ErrCredentialConflict, withCreds: true, wantHits: 1, wantForce: 1,
			wantMsg: openCodeGoConflict, absent: []string{"access-secret"},
		},
		{
			name: "401 workspace mismatch", status: http.StatusUnauthorized, body: `{}`,
			nextToken: "rotated-access", nextOrg: "org-2", wantHits: 1, wantForce: 1,
			wantMsg: openCodeGoConflict, absent: []string{"org-2", "rotated-access"},
		},
		{
			name: "sso", status: http.StatusForbidden, body: `{"_tag":"SsoRequired","detail":"secret-body"}`,
			wantHits: 1, wantMsg: "Sign in with SSO again to use Workspace.",
			absent: []string{"secret-body", "org-1", "SsoRequired"},
		},
		{
			name: "forbidden text is not sso", status: http.StatusForbidden, body: `{"message":"please use sso","detail":"secret-body"}`,
			wantHits: 1, wantMsg: "OpenCode Go is not enabled for Workspace.",
			absent: []string{"secret-body", "please use sso"},
		},
		{
			name: "server error", status: http.StatusInternalServerError, body: `{"error":"secret-detail"}`,
			wantHits: 1, wantMsg: openCodeGoTransient, absent: []string{"secret-detail", "500"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			var auths []string
			client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				n := hits.Add(1)
				auths = append(auths, r.Header.Get("Authorization"))
				if r.Header.Get("x-opencode-org-id") == "" {
					t.Error("missing org header")
				}
				status := tc.status
				body := tc.body
				if n == 2 && tc.nextToken != "" && tc.nextOrg == "" && tc.name == "401 then forbidden" {
					status = http.StatusForbidden
					body = `{"detail":"secret-body"}`
				}
				if n == 2 && tc.nextToken != "" && tc.name != "401 then forbidden" && tc.nextOrg == "" {
					status = http.StatusOK
					body = usageOK
				}
				return jsonResp(r, status, body), nil
			})}
			base := consoleUsageProvider(8, "user-1", "org-1", "stale-access", "refresh-1")
			base.OAuthCreds.OpencodeOrgName = "Workspace"
			stub := &credsStub{
				creds:              base.OAuthCreds,
				err:                tc.err,
				forceErr:           tc.forceErr,
				returnCredsWithErr: tc.withCreds,
			}
			if tc.nextToken != "" || tc.sameToken {
				next := *base.OAuthCreds
				if tc.nextToken != "" {
					next.AccessToken = tc.nextToken
					next.RefreshToken = "refresh-2"
				}
				if tc.nextOrg != "" {
					next.OpencodeOrgID = tc.nextOrg
				}
				stub.next = &next
			}
			p := consoleUsageProvider(8, "user-1", "org-1", "old-access", "refresh-0")
			p.OAuthCreds.OpencodeOrgName = "Workspace"
			svc := newService(stub, nil, client)
			rep, err := svc.Fetch(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if hits.Load() != tc.wantHits || stub.forceN != tc.wantForce {
				t.Fatalf("hits=%d force=%d, want hits=%d force=%d", hits.Load(), stub.forceN, tc.wantHits, tc.wantForce)
			}
			if rep.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", rep.Message, tc.wantMsg)
			}
			if len(rep.Quotas) != 0 && tc.wantMsg != "" {
				t.Fatalf("error class returned quotas: %+v", rep.Quotas)
			}
			for _, secret := range tc.absent {
				if strings.Contains(rep.Message, secret) || strings.Contains(rep.Plan, secret) {
					t.Fatalf("output contains %q: %+v", secret, rep)
				}
			}
			if tc.name == "terminal 401" {
				seen := stub.seenTokens()
				if len(seen) != 2 || seen[1] != "stale-access" {
					t.Fatalf("retry snapshot = %#v, want paired stale-access", seen)
				}
			}
			if tc.nextOrg != "" && len(auths) != 1 {
				t.Fatalf("mismatched workspace was sent: %#v", auths)
			}
		})
	}
}

func TestOpenCodeGo401RetriesPairedSnapshotOnce(t *testing.T) {
	var hits atomic.Int32
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		n := hits.Add(1)
		if r.Header.Get("x-opencode-org-id") != "org-1" {
			t.Errorf("org = %q", r.Header.Get("x-opencode-org-id"))
		}
		if n == 1 {
			if r.Header.Get("Authorization") != "Bearer stale-access" {
				t.Errorf("first auth = %q", r.Header.Get("Authorization"))
			}
			return jsonResp(r, http.StatusUnauthorized, `{"token":"stale-access"}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer rotated-access" {
			t.Errorf("second auth = %q", r.Header.Get("Authorization"))
		}
		return jsonResp(r, http.StatusOK, openCodeUsageBody(5, 5, 5, "ok", openCodeReset)), nil
	})}
	resolved := consoleUsageProvider(1, "user-1", "org-1", "stale-access", "refresh-1").OAuthCreds
	next := *resolved
	next.AccessToken = "rotated-access"
	next.RefreshToken = "refresh-2"
	stub := &credsStub{creds: resolved, next: &next}
	p := consoleUsageProvider(1, "user-1", "org-1", "old-access", "refresh-0")
	svc := newService(stub, nil, client)
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || len(rep.Quotas) != 3 {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if hits.Load() != 2 || stub.forceN != 1 {
		t.Fatalf("hits=%d force=%d", hits.Load(), stub.forceN)
	}
	seen := stub.seenTokens()
	if len(seen) != 2 || seen[0] != "old-access" || seen[1] != "stale-access" {
		t.Fatalf("resolver inputs = %#v", seen)
	}
}

func TestOpenCodeGoRefusesRedirectAndUnsafeURL(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var trips atomic.Int32
	var urls []string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		trips.Add(1)
		urls = append(urls, r.URL.String())
		h := make(http.Header)
		h.Set("Location", "http://evil.example/steal?access_token=access-secret")
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader("secret-body")),
			Request:    r,
		}, nil
	})}
	p := consoleUsageProvider(2, "user-1", "org-1", "access-secret", "refresh-secret")
	svc := newService(passthroughCreds{}, logger, client)
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if trips.Load() != 1 || len(urls) != 1 || urls[0] != opencode.GoUsageURL {
		t.Fatalf("trips=%d urls=%v", trips.Load(), urls)
	}
	if rep.Message != openCodeGoTransient || strings.Contains(rep.Message, "evil.example") || strings.Contains(rep.Message, "access-secret") || strings.Contains(rep.Message, "secret-body") {
		t.Fatalf("message = %q", rep.Message)
	}
	if strings.Contains(buf.String(), "access-secret") || strings.Contains(buf.String(), "org-1") || strings.Contains(buf.String(), "secret-body") {
		t.Fatalf("log leaked: %s", buf.String())
	}

	var unsafeHits atomic.Int32
	up := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		unsafeHits.Add(1)
		return jsonResp(r, 200, openCodeUsageBody(1, 1, 1, "ok", openCodeReset)), nil
	})}
	prev := opencode.GoUsageURL
	opencode.GoUsageURL = "http://127.0.0.1/usage"
	t.Cleanup(func() { opencode.GoUsageURL = prev })
	unsafeSvc := newService(passthroughCreds{}, nil, up)
	rep, err = unsafeSvc.Fetch(context.Background(), p)
	if err != nil || rep.Message != openCodeGoUnavailable || unsafeHits.Load() != 0 {
		t.Fatalf("unsafe err=%v msg=%q hits=%d", err, rep.Message, unsafeHits.Load())
	}
	if strings.Contains(rep.Message, "127.0.0.1") || strings.Contains(rep.Message, "access-secret") {
		t.Fatalf("unsafe message = %q", rep.Message)
	}
}

func TestOpenCodeGoTransportLogOmitsSecrets(t *testing.T) {
	const location = "https://upstream.example/private-upstream-detail%zz"
	for _, tc := range []struct {
		name      string
		transport rtFunc
	}{
		{"transport error", func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed for " + r.Header.Get("Authorization") + " org " + r.Header.Get("x-opencode-org-id"))
		}},
		{"malformed redirect", func(r *http.Request) (*http.Response, error) {
			resp := jsonResp(r, http.StatusFound, "")
			resp.Header.Set("Location", location)
			return resp, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			p := consoleUsageProvider(2, "acct-secret", "org-secret", "access-secret", "refresh-secret")
			svc := newService(passthroughCreds{}, logger, &http.Client{Transport: tc.transport})
			rep, err := svc.Fetch(t.Context(), p)
			if err != nil || rep == nil || rep.Message != openCodeGoTransient {
				t.Fatalf("err=%v report=%+v", err, rep)
			}
			logged := buf.String()
			if !strings.Contains(logged, "event=usage_transport_failed") {
				t.Fatal("transport failure metadata was not logged")
			}
			for _, secret := range []string{"access-secret", "refresh-secret", "org-secret", "acct-secret", "private-upstream-detail", "upstream.example"} {
				if strings.Contains(rep.Message, secret) || strings.Contains(logged, secret) {
					t.Fatalf("secret %q leaked in message or log", secret)
				}
			}
		})
	}
}

func TestOpenCodeGoCanceledFetchDoesNotCacheFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		warm bool
	}{
		{"first load", false},
		{"refresh with cached quotas", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			cancelHit := 1
			if tc.warm {
				cancelHit++
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				hits++
				if hits == cancelHit {
					cancel()
					return nil, r.Context().Err()
				}
				return jsonResp(r, http.StatusOK, openCodeUsageBody(25, 1, 1, "ok", openCodeReset)), nil
			})}
			svc := newService(passthroughCreds{}, nil, client)
			p := consoleUsageProvider(2, "account", "workspace", "access", "refresh")
			if tc.warm {
				if _, err := svc.Fetch(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}

			rep, err := svc.FetchWith(ctx, p, FetchOpts{Force: tc.warm})
			if rep != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled fetch returned report=%+v err=%v", rep, err)
			}
			rep, err = svc.Fetch(t.Context(), p)
			if err != nil || rep == nil || len(rep.Quotas) != 3 || *rep.Quotas[0].RemainingPct != 75 {
				t.Fatalf("next fetch returned report=%+v err=%v", rep, err)
			}
			if hits != 2 {
				t.Fatalf("HTTP calls = %d, want 2", hits)
			}
		})
	}
}

func TestOpenCodeGoReconnectDoesNotShareCacheOrInflight(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	var blockA atomic.Bool
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != opencode.GoUsageURL {
			t.Errorf("url = %s", r.URL.String())
		}
		org := r.Header.Get("x-opencode-org-id")
		hits.Add(1)
		if org == "org-a" && blockA.Load() {
			startOnce.Do(func() { close(started) })
			<-releaseCh
			return jsonResp(r, 200, openCodeUsageBody(99, 1, 1, "ok", openCodeReset)), nil
		}
		pct := 11.0
		if org == "org-b" {
			pct = 22
		}
		return jsonResp(r, 200, openCodeUsageBody(pct, 1, 1, "ok", openCodeReset)), nil
	})}
	svc := newService(passthroughCreds{}, nil, client)
	a := consoleUsageProvider(7, "acct-a", "org-a", "token-a", "refresh-a")
	b := consoleUsageProvider(7, "acct-b", "org-b", "token-b", "refresh-b")

	rep, err := svc.Fetch(context.Background(), a)
	if err != nil || *rep.Quotas[0].RemainingPct != 89 {
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
	if err != nil || *repB.Quotas[0].RemainingPct != 78 {
		t.Fatalf("B err=%v rep=%+v", err, repB)
	}
	if hits.Load() != 3 {
		t.Fatalf("B joined A or missed, hits=%d", hits.Load())
	}
	if strings.Contains(repB.Plan, "org-a") || strings.Contains(repB.Message, "token-a") {
		t.Fatalf("B report leaked A: %+v", repB)
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
	if err != nil || *repB.Quotas[0].RemainingPct != 78 || hits.Load() != 3 {
		t.Fatalf("late A clobbered B: %+v hits=%d err=%v", repB, hits.Load(), err)
	}
	blockA.Store(false)
	repA, err := svc.Fetch(context.Background(), a)
	if err != nil || *repA.Quotas[0].RemainingPct != 89 {
		t.Fatalf("A after reconnect = %+v err=%v", repA, err)
	}
	if hits.Load() != 4 {
		t.Fatalf("A did not fetch its own report, hits=%d", hits.Load())
	}
}

func TestOpenCodeGoChangedGrantDoesNotJoinOrClobber(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		auth := r.Header.Get("Authorization")
		if auth == "Bearer token-g1" {
			startOnce.Do(func() { close(started) })
			<-releaseCh
			return jsonResp(r, 200, openCodeUsageBody(90, 1, 1, "ok", openCodeReset)), nil
		}
		return jsonResp(r, 200, openCodeUsageBody(40, 1, 1, "ok", openCodeReset)), nil
	})}
	svc := newService(passthroughCreds{}, nil, client)
	g1 := consoleUsageProvider(9, "user-1", "org-1", "token-g1", "refresh-g1")
	g2 := consoleUsageProvider(9, "user-1", "org-1", "token-g2", "refresh-g2")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.Fetch(context.Background(), g1)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("g1 did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rep, err := svc.Fetch(ctx, g2)
	if err != nil || *rep.Quotas[0].RemainingPct != 60 {
		t.Fatalf("g2 err=%v rep=%+v", err, rep)
	}
	release()
	<-done
	rep, err = svc.Fetch(context.Background(), g2)
	if err != nil || *rep.Quotas[0].RemainingPct != 60 || hits.Load() != 2 {
		t.Fatalf("late g1 clobbered g2: %+v hits=%d err=%v", rep, hits.Load(), err)
	}
}

func TestOpenCodeGoTokenRefreshReusesCacheAndFailureStaysWithGrant(t *testing.T) {
	var hits atomic.Int32
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer token-old", "Bearer token-new":
			return jsonResp(r, 200, openCodeUsageBody(15, 1, 1, "ok", openCodeReset)), nil
		case "Bearer token-other":
			return jsonResp(r, 200, openCodeUsageBody(4, 1, 1, "ok", openCodeReset)), nil
		}
		return jsonResp(r, http.StatusInternalServerError, `{"error":"secret-detail"}`), nil
	})}
	now := time.Now()
	svc := newService(passthroughCreds{}, nil, client)
	svc.now = func() time.Time { return now }
	p := consoleUsageProvider(5, "user-1", "org-1", "token-old", "refresh-old")
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || *rep.Quotas[0].RemainingPct != 85 {
		t.Fatalf("first err=%v rep=%+v", err, rep)
	}
	p.OAuthCreds.AccessToken = "token-new"
	p.OAuthCreds.RefreshToken = "refresh-new"
	if _, err := svc.Fetch(context.Background(), p); err != nil || hits.Load() != 1 {
		t.Fatalf("token refresh missed cache, hits=%d err=%v", hits.Load(), err)
	}
	if _, err := svc.FetchWith(context.Background(), p, FetchOpts{Force: true}); err != nil || hits.Load() != 2 {
		t.Fatalf("force hits=%d err=%v", hits.Load(), err)
	}

	fail := consoleUsageProvider(6, "user-1", "org-9", "token-fail", "refresh-fail")
	failRep, err := svc.Fetch(context.Background(), fail)
	if err != nil || failRep.Message != openCodeGoTransient || strings.Contains(failRep.Message, "secret-detail") {
		t.Fatalf("failure = %+v err=%v", failRep, err)
	}
	before := hits.Load()
	if _, err := svc.Fetch(context.Background(), fail); err != nil || hits.Load() != before {
		t.Fatalf("same grant did not reuse failure cache, hits=%d err=%v", hits.Load(), err)
	}
	fail.OAuthCreds.AccessToken = "token-other"
	fail.OAuthCreds.RefreshToken = "refresh-other"
	other, err := svc.Fetch(context.Background(), fail)
	if err != nil || other.Message != "" || len(other.Quotas) == 0 || hits.Load() != before+1 {
		t.Fatalf("new grant inherited failure: %+v hits=%d err=%v", other, hits.Load(), err)
	}
}

func TestOpenCodeGoIgnoresGenericProviderCache(t *testing.T) {
	var hits atomic.Int32
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResp(r, 200, openCodeUsageBody(3, 3, 3, "ok", openCodeReset)), nil
	})}
	svc := newService(passthroughCreds{}, nil, client)
	p := consoleUsageProvider(12, "user-1", "org-1", "token", "refresh")
	svc.cache[p.ID] = cacheEntry{report: &Report{Plan: "stale-codex", Message: "nope"}, expiresAt: time.Now().Add(time.Minute)}
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || rep.Plan == "stale-codex" || len(rep.Quotas) != 3 || hits.Load() != 1 {
		t.Fatalf("generic cache leaked: %+v hits=%d err=%v", rep, hits.Load(), err)
	}
	other := consoleUsageProvider(13, "user-1", "org-1", "token", "refresh")
	if _, err := svc.Fetch(context.Background(), other); err != nil || hits.Load() != 2 {
		t.Fatalf("provider ids shared cache, hits=%d err=%v", hits.Load(), err)
	}
	api := *p
	api.AuthMethod = domain.AuthAPIKey
	api.APIKey = "go-secret"
	api.OAuthCreds = nil
	if _, err := svc.Fetch(context.Background(), &api); err != ErrUnsupported || hits.Load() != 2 {
		t.Fatalf("mode change returned cached console report: %v hits=%d", err, hits.Load())
	}
}
