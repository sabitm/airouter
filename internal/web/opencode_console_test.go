package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/opencode"
)

type stubConnector struct {
	state string
	creds *domain.OAuthCreds
	err   error
	done  bool
}

func (s stubConnector) State() string { return s.state }
func (s stubConnector) Result() (*domain.OAuthCreds, error, bool) {
	return s.creds, s.err, s.done
}
func (s stubConnector) Close() error { return nil }

func consoleCreds(org, name string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode:                domain.OAuthAuto,
		Preset:              "opencode-console",
		AccessToken:         "access-secret",
		RefreshToken:        "refresh-secret",
		ExpiresAt:           1_800,
		Email:               "user@example.com",
		AccountID:           "user-1",
		ClientID:            opencode.ConsoleClientID,
		TokenURL:            "https://opencode.ai/console/auth/device/token",
		RefreshJSON:         true,
		OpencodeConsoleAuth: true,
		OpencodeOrgID:       org,
		OpencodeOrgName:     name,
	}
}

func TestOpencodeConsoleSessionClaim(t *testing.T) {
	h := testHandler(t)
	now := timeNow()
	h.sessions.putBound("create", stubConnector{state: "create", creds: consoleCreds("org-1", "Workspace"), done: true}, opencodeConsoleFlow, 0, now)
	h.sessions.putBound("edit-5", stubConnector{state: "edit-5", creds: consoleCreds("org-5", "Five"), done: true}, opencodeConsoleFlow, 5, now)
	h.sessions.put("other", &connectSession{conn: stubConnector{state: "other", creds: consoleCreds("org-x", "X"), done: true}, created: now}, now)

	if _, ok := h.sessions.claimOpencodeConsole("edit-5", 0); ok {
		t.Fatal("edit session was claimed for create")
	}
	if _, ok := h.sessions.claimOpencodeConsole("create", 5); ok {
		t.Fatal("create session was claimed for an edit")
	}
	if _, ok := h.sessions.claimOpencodeConsole("other", 0); ok {
		t.Fatal("unbound session was claimed")
	}
	creds, ok := h.sessions.claimOpencodeConsole("create", 0)
	if !ok || creds.OpencodeOrgID != "org-1" {
		t.Fatal("first claim failed")
	}
	if _, ok := h.sessions.claimOpencodeConsole("create", 0); ok {
		t.Fatal("second claim succeeded")
	}
	h.sessions.releaseClaim("create")
	if _, ok := h.sessions.claimOpencodeConsole("create", 0); !ok {
		t.Fatal("failed write did not release the claim")
	}
}

func TestOpencodeConsoleExpiredAttempt(t *testing.T) {
	h := testHandler(t)
	now := time.Now()
	for _, state := range []string{"status", "check", "save"} {
		h.sessions.put(state, &connectSession{
			conn:    stubConnector{state: state, creds: consoleCreds("org-1", "Workspace"), done: true},
			created: now.Add(-connectTTL), flow: opencodeConsoleFlow, bound: true,
		}, now.Add(-connectTTL))
	}
	withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("expired attempt reached upstream")
		return nil, nil
	})
	rec := httptest.NewRecorder()
	h.oauthConnectStatus(rec, httptest.NewRequest(http.MethodGet, "/dashboard/providers/oauth/status?state=status", nil))
	if !strings.Contains(rec.Body.String(), "connect session expired") {
		t.Fatalf("status: %s", rec.Body.String())
	}
	form := opencodeProviderForm("go", "")
	form.Set("opencode_auth", "console")
	form.Set("oauth_session", "check")
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "sign in with OpenCode Console before checking") {
		t.Fatalf("check: %s", rec.Body.String())
	}
	form.Set("oauth_session", "save")
	rec = httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sign in with OpenCode Console before saving") {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateOpencodeConsoleProvider(t *testing.T) {
	h := testHandler(t)
	h.sessions.putBound("ok", stubConnector{state: "ok", creds: consoleCreds("org-1", "Workspace"), done: true}, opencodeConsoleFlow, 0, timeNow())
	form := opencodeProviderForm("go", "should-not-stick")
	form.Set("opencode_auth", "console")
	form.Set("oauth_session", "ok")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "access-secret") || strings.Contains(rec.Body.String(), "refresh-secret") {
		t.Fatalf("dashboard showed a token: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Workspace") {
		t.Fatalf("dashboard did not show the workspace: %s", rec.Body.String())
	}
	providers, err := h.store.ListProviders(context.Background())
	if err != nil || len(providers) != 1 {
		t.Fatalf("providers=%v err=%v", providers, err)
	}
	p := providers[0]
	if p.Method() != domain.AuthOAuth || p.APIKey != "" || p.BaseURL != opencode.GoBaseURL || p.OAuthCreds.OpencodeOrgName != "Workspace" {
		t.Fatalf("provider=%+v creds=%+v", p, p.OAuthCreds)
	}
	if _, ok := h.sessions.get("ok"); ok {
		t.Fatal("successful save left the session usable")
	}

	h.sessions.putBound("again", stubConnector{state: "again", creds: consoleCreds("org-2", "Other"), done: true}, opencodeConsoleFlow, 9, timeNow())
	form.Set("oauth_session", "again")
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-target create status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateOpencodeConsoleModes(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "go", BaseURL: opencode.GoBaseURL, APIKey: "go-secret", Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthBearer,
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h.sessions.putBound("reconnect", stubConnector{state: "reconnect", creds: consoleCreds("org-1", "Workspace"), done: true}, opencodeConsoleFlow, p.ID, timeNow())
	form := opencodeProviderForm("go", "ignored")
	form.Set("opencode_auth", "console")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	form.Set("oauth_session", "reconnect")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconnect status=%d body=%s form=%s", rec.Code, rec.Body.String(), form.Encode())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method() != domain.AuthOAuth || got.APIKey != "" || got.OAuthCreds.OpencodeOrgID != "org-1" {
		t.Fatalf("reconnected=%+v creds=%+v", got, got.OAuthCreds)
	}
	stored, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	form = opencodeProviderForm("go", "")
	form.Set("opencode_auth", "console")
	form.Set("name", "renamed")
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec = httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status=%d body=%s", rec.Code, rec.Body.String())
	}
	after, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuthBlob != stored.OAuthBlob || after.Provider.Name != "renamed" {
		t.Fatal("metadata edit rewrote credentials or skipped the name")
	}

	rec = updateOpencodeProvider(t, h, p.ID, "zen", "paid")
	if rec.Code != http.StatusOK {
		t.Fatalf("zen switch status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ = h.store.GetProvider(context.Background(), p.ID)
	if got.Method() != domain.AuthAPIKey || got.APIKey != opencode.PublicKey || got.BaseURL != opencode.ZenBaseURL || got.OAuthCreds != nil {
		t.Fatalf("zen switch left console creds: %+v creds=%+v", got, got.OAuthCreds)
	}
}

func TestOpencodeConsoleCheckUsesUsageNotModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/device/token" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":600}`)
	}))
	defer srv.Close()
	prevOrigin := opencode.ConsoleOrigin
	opencode.ConsoleOrigin = srv.URL
	t.Cleanup(func() { opencode.ConsoleOrigin = prevOrigin })
	restoreClient := oauth.SetOpencodeConsoleClientForTest(srv.Client())
	t.Cleanup(restoreClient)
	h := testHandler(t)
	p := &domain.Provider{
		Name: "console", BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: consoleCreds("org-1", "Workspace"),
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var gotURL, gotAuth, gotOrg string
	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		gotAuth = req.Header.Get("Authorization")
		gotOrg = req.Header.Get("x-opencode-org-id")
		if strings.Contains(req.URL.Path, "/models") {
			t.Errorf("check called models: %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"usage":{"rolling":{"status":"ok"},"weekly":{"status":"ok"},"monthly":{"status":"rate-limited"}}}`)),
			Request:    req,
		}, nil
	})
	form := opencodeProviderForm("go", "")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	form.Set("opencode_auth", "console")
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "authorized for Workspace") {
		t.Fatalf("check=%s", rec.Body.String())
	}
	if gotURL != opencode.GoUsageURL || gotAuth != "Bearer rotated-access" || gotOrg != "org-1" {
		t.Fatalf("usage request url=%q auth=%q org=%q", gotURL, gotAuth, gotOrg)
	}

	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"_tag":"SsoRequired"}`)), Request: req}, nil
	})
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "SSO") || strings.Contains(rec.Body.String(), "reconnect required") {
		t.Fatalf("sso check=%s", rec.Body.String())
	}

	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
	})
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "not enabled") {
		t.Fatalf("entitlement check=%s", rec.Body.String())
	}

	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"_tag":"Unauthorized"}`)), Request: req}, nil
	})
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "reconnect required") {
		t.Fatalf("reconnect check=%s", rec.Body.String())
	}

	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "transient") || strings.Contains(rec.Body.String(), "reconnect") {
		t.Fatalf("transient check=%s", rec.Body.String())
	}
}

func TestOpenCodeCheckUsesSelectedAuthMode(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "console", BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthOAuth, OAuthCreds: consoleCreds("org-1", "Workspace"),
	}
	if err := h.store.CreateProvider(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ tier, key, endpoint, bearer string }{
		{"go", "new-key", opencode.GoBaseURL + "/models", "Bearer new-key"},
		{"zen", "", opencode.ZenBaseURL + "/models", "Bearer public"},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			calls := 0
			withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != tc.endpoint || req.Header.Get("Authorization") != tc.bearer {
					t.Errorf("unexpected check request: %s", req.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"m"}]}`)), Request: req}, nil
			})
			form := opencodeProviderForm(tc.tier, tc.key)
			form.Set("id", strconv.FormatInt(p.ID, 10))
			form.Set("auth_method", "oauth") // Hidden field still describes the saved row.
			form.Set("opencode_auth", "apikey")
			form.Set("oauth_session", "unused-console-state")
			rec := httptest.NewRecorder()
			h.checkProvider(rec, reqWithForm(form))
			if calls != 1 || !strings.Contains(rec.Body.String(), "OK") {
				t.Fatalf("calls=%d check=%s", calls, rec.Body.String())
			}
		})
	}
}

func TestOpencodeConsoleBeginAndChoice(t *testing.T) {
	var muToken int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/device/code":
			_, _ = io.WriteString(w, `{"device_code":"dev-secret","user_code":"USER","verification_uri_complete":"/device?user_code=USER","expires_in":60,"interval":1}`)
		case "/auth/device/token":
			muToken++
			if muToken == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"access-secret","refresh_token":"refresh-secret","expires_in":60}`)
		case "/api/user":
			_, _ = io.WriteString(w, `{"id":"user","email":"user@example.com"}`)
		case "/api/orgs":
			_, _ = io.WriteString(w, `[{"id":"org-a","name":"Alpha"},{"id":"org-b","name":"Beta"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	prev := opencode.ConsoleOrigin
	opencode.ConsoleOrigin = srv.URL
	t.Cleanup(func() { opencode.ConsoleOrigin = prev })
	restoreClient := oauth.SetOpencodeConsoleClientForTest(srv.Client())
	t.Cleanup(restoreClient)
	restoreWait := oauth.SetOpencodeConsoleWaitForTest(func(context.Context, time.Duration) error { return nil })
	t.Cleanup(restoreWait)

	h := testHandler(t)
	form := opencodeProviderForm("go", "")
	form.Set("opencode_auth", "console")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/opencode/console/begin", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.opencodeConsoleBegin(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "dev-secret") || strings.Contains(body, "access-secret") {
		t.Fatalf("begin leaked a secret: %s", body)
	}
	if !strings.Contains(body, "USER") || !strings.Contains(body, srv.URL+"/device?user_code=USER") {
		t.Fatalf("begin view=%s", body)
	}
	state := sessionStateFrom(t, body)
	deadline := timeNow().Add(2 * time.Second)
	var status string
	for timeNow().Before(deadline) {
		sreq := httptest.NewRequest(http.MethodGet, "/dashboard/providers/oauth/status?state="+url.QueryEscape(state), nil)
		srec := httptest.NewRecorder()
		h.oauthConnectStatus(srec, sreq)
		status = srec.Body.String()
		if strings.Contains(status, "opencode_org_choice") {
			break
		}
		if strings.Contains(status, "dev-secret") || strings.Contains(status, "access-secret") {
			t.Fatalf("status leaked a secret: %s", status)
		}
	}
	if !strings.Contains(status, "Alpha") || !strings.Contains(status, "Beta") {
		t.Fatalf("choice status=%s", status)
	}
	choice := url.Values{}
	choice.Set("state", state)
	choice.Set("opencode_org_choice", "org-b")
	creq := httptest.NewRequest(http.MethodPost, "/dashboard/providers/opencode/console/org", strings.NewReader(choice.Encode()))
	creq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crec := httptest.NewRecorder()
	h.opencodeConsoleChoose(crec, creq)
	if !strings.Contains(crec.Body.String(), "Beta") || strings.Contains(crec.Body.String(), "access-secret") {
		t.Fatalf("choice result=%s", crec.Body.String())
	}
	save := opencodeProviderForm("go", "pasted")
	save.Set("opencode_auth", "console")
	save.Set("oauth_session", state)
	sreq := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(save.Encode()))
	sreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srec := httptest.NewRecorder()
	h.createProvider(srec, sreq)
	if srec.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", srec.Code, srec.Body.String())
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 1 || providers[0].OAuthCreds.OpencodeOrgID != "org-b" || providers[0].APIKey != "" {
		t.Fatalf("saved=%+v", providers)
	}
	sreq = httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(save.Encode()))
	sreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srec = httptest.NewRecorder()
	h.createProvider(srec, sreq)
	if srec.Code == http.StatusOK {
		t.Fatal("claimed session was reusable")
	}
}

func TestOpencodeConsoleSavedRefresh(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "console", BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: consoleCreds("org-1", "Workspace"),
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":30}`)
	}))
	defer srv.Close()
	prev := opencode.ConsoleOrigin
	opencode.ConsoleOrigin = srv.URL
	t.Cleanup(func() { opencode.ConsoleOrigin = prev })
	restoreClient := oauth.SetOpencodeConsoleClientForTest(srv.Client())
	t.Cleanup(restoreClient)
	form := url.Values{}
	form.Set("id", strconv.FormatInt(p.ID, 10))
	form.Set("refresh_view", "status")
	form.Set("refresh_token", "pasted-must-not-be-sent")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/oauth/refresh", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, req)
	if !strings.Contains(rec.Body.String(), "refreshed") || strings.Contains(rec.Body.String(), "rotated-access") {
		t.Fatalf("refresh view=%s", rec.Body.String())
	}
	if strings.Contains(gotBody, "pasted-must-not-be-sent") || !strings.Contains(gotBody, "refresh-secret") {
		t.Fatalf("refresh body=%s", gotBody)
	}
	got, _ := h.store.GetProvider(context.Background(), p.ID)
	if got.OAuthCreds.AccessToken != "rotated-access" || got.OAuthCreds.OpencodeOrgID != "org-1" || got.OAuthCreds.OpencodeOrgName != "Workspace" {
		t.Fatalf("saved refresh=%+v", got.OAuthCreds)
	}
}

func sessionStateFrom(t *testing.T, body string) string {
	t.Helper()
	const key = "state="
	i := strings.Index(body, key)
	if i < 0 {
		t.Fatalf("no state in %s", body)
	}
	rest := body[i+len(key):]
	end := strings.IndexAny(rest, "\"&< ")
	if end < 0 {
		end = len(rest)
	}
	state, err := url.QueryUnescape(rest[:end])
	if err != nil {
		t.Fatal(err)
	}
	return state
}
