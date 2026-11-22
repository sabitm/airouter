package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
)

// These tests swap global transports serially. Only exact fixture URLs may
// reach a socket, including redirects and a regression to public OAuth URLs.
func withDashboardLocalNetwork(t *testing.T, allowed ...string) {
	t.Helper()
	previous := http.DefaultTransport
	base, ok := previous.(*http.Transport)
	if !ok {
		t.Fatal("default transport is not *http.Transport")
	}
	local := base.Clone()
	local.Proxy = nil
	http.DefaultTransport = opencodeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		for _, endpoint := range allowed {
			if req.URL.String() == endpoint {
				return local.RoundTrip(req)
			}
		}
		t.Errorf("blocked unexpected dashboard request: %s %s", req.Method, req.URL.Redacted())
		return nil, fmt.Errorf("unexpected dashboard test endpoint")
	})
	t.Cleanup(func() {
		http.DefaultTransport = previous
		local.CloseIdleConnections()
	})
}

type dashboardProbe struct {
	method, url, auth, beta string
	helper                  bool
}

func captureDashboardProbe(t *testing.T, hits *atomic.Int32) chan dashboardProbe {
	t.Helper()
	ch := make(chan dashboardProbe, 8)
	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		if hits != nil {
			hits.Add(1)
		}
		_, helper := req.Header["X-Stainless-Helper-Method"]
		select {
		case ch <- dashboardProbe{
			method: req.Method, url: req.URL.String(), auth: req.Header.Get("Authorization"),
			beta: req.Header.Get("anthropic-beta"), helper: helper,
		}:
		default:
			t.Errorf("probe channel full for %s", req.URL.Redacted())
		}
		return opencodeModelsResponse(), nil
	})
	return ch
}

func newClaudeModelsServer(t *testing.T, hits *atomic.Int32, status int, body string) (*httptest.Server, <-chan dashboardProbe) {
	t.Helper()
	ch := make(chan dashboardProbe, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/models?limit=1000" {
			t.Errorf("models request = %s %s", r.Method, r.URL.RequestURI())
		}
		_, helper := r.Header["X-Stainless-Helper-Method"]
		if helper {
			t.Error("models probe claimed an SDK helper")
		}
		ch <- dashboardProbe{method: r.Method, url: r.URL.String(), auth: r.Header.Get("Authorization"), beta: r.Header.Get("anthropic-beta"), helper: helper}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func takeDashboardProbe(t *testing.T, ch <-chan dashboardProbe) dashboardProbe {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for dashboard probe")
		return dashboardProbe{}
	}
}

func assertNoExtraProbe(t *testing.T, ch <-chan dashboardProbe) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("unexpected extra probe %s %s", got.method, got.url)
	case <-time.After(50 * time.Millisecond):
	}
}

func claudeTokenHandler(t *testing.T, hits *atomic.Int32, previousScopes, access, refresh, scopes, account string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/" {
			t.Errorf("token request = %s %s", r.Method, r.URL.RequestURI())
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("token body: %v", err)
		}
		if payload["grant_type"] != "refresh_token" || payload["refresh_token"] != "rt-old" || payload["scope"] != previousScopes || payload["client_id"] != "cid-synthetic" {
			t.Errorf("token payload = %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "refresh_token": refresh, "expires_in": 3600,
			"scope": scopes, "account": map[string]string{"uuid": account},
		})
	}
}

func runClaudeDashboardPolicy(t *testing.T, h *Handler, p *domain.Provider, entrypoint string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	switch entrypoint {
	case "check":
		form := url.Values{
			"auth_method": {"oauth"}, "protocol": {string(domain.ProtocolClaudeCode)},
			"base_url": {p.BaseURL}, "id": {strconv.FormatInt(p.ID, 10)},
		}
		h.checkProvider(rec, reqWithForm(form).WithContext(ctx))
	case "autocomplete":
		req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/models?provider_id="+strconv.FormatInt(p.ID, 10), nil)
		h.providerModels(rec, req.WithContext(ctx))
	default:
		t.Fatalf("unknown dashboard entrypoint %q", entrypoint)
	}
	return rec
}

func TestClaudeCodeSavedCheckAndAutocompleteUsePairedSnapshot(t *testing.T) {
	for _, direction := range []struct {
		name, oldToken, oldScopes, newToken, newScopes, wantBeta string
	}{
		{"profile to inference", "sk-ant-oat-stale", "user:profile", "opaque-refreshed", "user:inference", claudecode.BetaOAuth},
		{"inference to profile", "opaque-stale", "user:inference", "sk-ant-oat-refreshed", "user:profile", ""},
	} {
		for _, entrypoint := range []string{"check", "autocomplete"} {
			t.Run(direction.name+"/"+entrypoint, func(t *testing.T) {
				h := testHandler(t)
				var tokenHits atomic.Int32
				tokenSrv := httptest.NewServer(claudeTokenHandler(t, &tokenHits, direction.oldScopes, direction.newToken, "rt-next", direction.newScopes, "acct-refreshed"))
				t.Cleanup(tokenSrv.Close)
				var checkHits, modelHits atomic.Int32
				checks := captureDashboardProbe(t, &checkHits)
				models, modelProbes := newClaudeModelsServer(t, &modelHits, http.StatusOK, `{"data":[{"id":"account-model"}]}`)
				withDashboardLocalNetwork(t, tokenSrv.URL, models.URL+"/models?limit=1000")
				p := &domain.Provider{
					Name: "claude-saved", BaseURL: models.URL + "/", Protocol: domain.ProtocolClaudeCode,
					AuthMethod: domain.AuthOAuth,
					OAuthCreds: &domain.OAuthCreds{
						ClaudeCodeAuth: true, AccessToken: direction.oldToken, RefreshToken: "rt-old", ClientID: "cid-synthetic",
						ClaudeScopes: direction.oldScopes, ClaudeAccountUUID: "acct-stale", ClaudeDeviceID: strings.Repeat("ab", 32),
						ExpiresAt: time.Now().Add(time.Minute).Unix(), TokenURL: tokenSrv.URL,
					},
				}
				if err := h.store.CreateProvider(context.Background(), p); err != nil {
					t.Fatal(err)
				}
				before := *p.OAuthCreds
				rec := runClaudeDashboardPolicy(t, h, p, entrypoint)
				var got dashboardProbe
				if entrypoint == "check" {
					if !strings.Contains(rec.Body.String(), "does not prove Messages") || checkHits.Load() != 1 || modelHits.Load() != 0 {
						t.Fatalf("check result %s check hits %d model hits %d", rec.Body.String(), checkHits.Load(), modelHits.Load())
					}
					got = takeDashboardProbe(t, checks)
					if got.url != models.URL+"/models?limit=1000" {
						t.Fatalf("check URL = %q", got.url)
					}
				} else {
					if !strings.Contains(rec.Body.String(), `value="account-model"`) || checkHits.Load() != 0 || modelHits.Load() != 1 {
						t.Fatalf("autocomplete result %s check hits %d model hits %d", rec.Body.String(), checkHits.Load(), modelHits.Load())
					}
					got = takeDashboardProbe(t, modelProbes)
				}
				if got.method != http.MethodGet || got.auth != "Bearer "+direction.newToken || got.beta != direction.wantBeta || got.helper {
					t.Fatalf("resolved probe = %+v, want beta %q", got, direction.wantBeta)
				}
				if tokenHits.Load() != 1 {
					t.Fatalf("token hits = %d, want 1", tokenHits.Load())
				}
				saved, err := h.store.GetProvider(context.Background(), p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if saved.OAuthCreds.AccessToken != direction.newToken || saved.OAuthCreds.RefreshToken != "rt-next" || saved.OAuthCreds.ClaudeScopes != direction.newScopes || saved.OAuthCreds.ClaudeAccountUUID != "acct-refreshed" || saved.OAuthCreds.ClaudeDeviceID != before.ClaudeDeviceID {
					t.Fatalf("saved snapshot = %+v", saved.OAuthCreds)
				}
				if !reflect.DeepEqual(*p.OAuthCreds, before) {
					t.Fatal("request changed the caller's credential snapshot")
				}
				assertNoExtraProbe(t, checks)
				assertNoExtraProbe(t, modelProbes)
			})
		}
	}
}

// Handler reads use the real store. This wrapper changes the row only when
// ResolveCreds reads it, after the handler has captured its stale snapshot.
type dashboardResolutionStore struct {
	oauth.ProviderStore
	beforeRead func(context.Context, int64) error
}

func (s dashboardResolutionStore) GetProvider(ctx context.Context, id int64) (*domain.Provider, error) {
	if err := s.beforeRead(ctx, id); err != nil {
		return nil, err
	}
	return s.ProviderStore.GetProvider(ctx, id)
}

func TestClaudeCodeDashboardUsesNewerStoredSnapshot(t *testing.T) {
	for _, direction := range []struct {
		name, oldToken, oldScopes, newToken, newScopes, wantBeta string
	}{
		{"profile to inference", "sk-ant-oat-stale", "user:profile", "opaque-current", "user:inference", claudecode.BetaOAuth},
		{"inference to profile", "opaque-stale", "user:inference", "sk-ant-oat-current", "user:profile", ""},
	} {
		for _, entrypoint := range []string{"check", "autocomplete"} {
			t.Run(direction.name+"/"+entrypoint, func(t *testing.T) {
				h := testHandler(t)
				var tokenHits atomic.Int32
				tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					tokenHits.Add(1)
					http.Error(w, "refresh must not run", http.StatusBadGateway)
				}))
				t.Cleanup(tokenSrv.Close)
				var checkHits, modelHits atomic.Int32
				checks := captureDashboardProbe(t, &checkHits)
				models, modelProbes := newClaudeModelsServer(t, &modelHits, http.StatusOK, `{"data":[{"id":"current-model"}]}`)
				withDashboardLocalNetwork(t, tokenSrv.URL, models.URL+"/models?limit=1000")
				stale := &domain.Provider{
					Name: "claude-newer", BaseURL: models.URL, Protocol: domain.ProtocolClaudeCode,
					AuthMethod: domain.AuthOAuth,
					OAuthCreds: &domain.OAuthCreds{
						ClaudeCodeAuth: true, AccessToken: direction.oldToken, RefreshToken: "rt-old", ClientID: "cid-synthetic",
						ClaudeScopes: direction.oldScopes, ClaudeAccountUUID: "acct-stale", ClaudeDeviceID: strings.Repeat("cd", 32),
						ExpiresAt: time.Now().Add(time.Minute).Unix(), TokenURL: tokenSrv.URL,
					},
				}
				if err := h.store.CreateProvider(context.Background(), stale); err != nil {
					t.Fatal(err)
				}
				before := *stale.OAuthCreds
				current := before
				current.AccessToken = direction.newToken
				current.ClaudeScopes = direction.newScopes
				current.ClaudeAccountUUID = "acct-current"
				current.ExpiresAt = time.Now().Add(time.Hour).Unix()
				var resolutionReads atomic.Int32
				h.oauth = oauth.New(dashboardResolutionStore{
					ProviderStore: h.store,
					beforeRead: func(ctx context.Context, id int64) error {
						if id != stale.ID || resolutionReads.Add(1) != 1 {
							t.Errorf("unexpected resolution read for provider %d", id)
							return fmt.Errorf("unexpected resolution read")
						}
						stored, err := h.store.GetProvider(ctx, id)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(*stored.OAuthCreds, before) {
							t.Error("row changed before the resolution boundary")
						}
						return h.store.UpdateProviderOAuth(ctx, id, &current)
					},
				})
				rec := runClaudeDashboardPolicy(t, h, stale, entrypoint)
				var got dashboardProbe
				if entrypoint == "check" {
					if !strings.Contains(rec.Body.String(), "does not prove Messages") || checkHits.Load() != 1 || modelHits.Load() != 0 {
						t.Fatalf("check result %s check hits %d model hits %d", rec.Body.String(), checkHits.Load(), modelHits.Load())
					}
					got = takeDashboardProbe(t, checks)
				} else {
					if !strings.Contains(rec.Body.String(), `value="current-model"`) || checkHits.Load() != 0 || modelHits.Load() != 1 {
						t.Fatalf("autocomplete result %s check hits %d model hits %d", rec.Body.String(), checkHits.Load(), modelHits.Load())
					}
					got = takeDashboardProbe(t, modelProbes)
				}
				if got.method != http.MethodGet || got.auth != "Bearer "+direction.newToken || got.beta != direction.wantBeta || got.helper {
					t.Fatalf("newer snapshot probe = %+v, want beta %q", got, direction.wantBeta)
				}
				if tokenHits.Load() != 0 || resolutionReads.Load() != 1 {
					t.Fatalf("token hits %d resolution reads %d", tokenHits.Load(), resolutionReads.Load())
				}
				saved, err := h.store.GetProvider(context.Background(), stale.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(*saved.OAuthCreds, current) || !reflect.DeepEqual(*stale.OAuthCreds, before) {
					t.Fatal("newer stored or stale caller snapshot was changed")
				}
				assertNoExtraProbe(t, checks)
				assertNoExtraProbe(t, modelProbes)
			})
		}
	}
}

func TestInstallResolvedClaudeCredsCopiesSnapshot(t *testing.T) {
	source := domain.OAuthCreds{
		AccessToken: "opaque-current", ClaudeScopes: "user:inference",
		ClaudeAccountUUID: "acct-current", ClaudeDeviceID: strings.Repeat("ab", 32),
	}
	before := source
	p := &domain.Provider{APIKey: "old-token", OAuthCreds: &domain.OAuthCreds{AccessToken: "old-token"}}
	installResolvedClaudeCreds(p, &source)
	if p.APIKey != source.AccessToken || p.OAuthCreds == &source || !reflect.DeepEqual(*p.OAuthCreds, before) || !reflect.DeepEqual(source, before) {
		t.Fatal("installer did not copy the paired credential snapshot")
	}
	p.OAuthCreds.ClaudeScopes = "user:profile"
	p.OAuthCreds.AccessToken = "request-local-token"
	if !reflect.DeepEqual(source, before) {
		t.Fatal("request-local changes mutated the source")
	}
}

func TestClaudeCodeUnsavedCheckKeepsSessionAndManualCredentials(t *testing.T) {
	withDashboardLocalNetwork(t)
	h := testHandler(t)
	var tokenHits atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits.Add(1)
		http.Error(w, "unsaved check must not refresh", http.StatusBadGateway)
	}))
	t.Cleanup(tokenSrv.Close)
	var probeHits atomic.Int32
	probes := captureDashboardProbe(t, &probeHits)

	state := "synthetic-session"
	h.sessions.put(state, &connectSession{conn: presetConnector{creds: &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "opaque-session", RefreshToken: "rt-session",
		ClaudeScopes: "user:inference", ClaudeAccountUUID: "acct-session",
	}}, created: time.Now()}, time.Now())

	form := url.Values{
		"auth_method": {"oauth"}, "protocol": {string(domain.ProtocolClaudeCode)},
		"base_url": {"https://claude-session.invalid/v1/"}, "oauth_session": {state},
	}
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	got := takeDashboardProbe(t, probes)
	if !strings.Contains(rec.Body.String(), "does not prove Messages") || got.auth != "Bearer opaque-session" || got.beta != claudecode.BetaOAuth || got.url != "https://claude-session.invalid/v1/models?limit=1000" || got.helper {
		t.Fatalf("session check %s probe %+v", rec.Body.String(), got)
	}

	manual := url.Values{
		"auth_method": {"oauth"}, "protocol": {string(domain.ProtocolClaudeCode)}, "preset": {"custom"},
		"base_url": {"https://claude-manual.invalid/v1"}, "token_url": {tokenSrv.URL}, "client_id": {"cid-synthetic"},
		"access_token": {"sk-ant-oat-manual"}, "refresh_token": {"rt-manual"},
	}
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(manual))
	got = takeDashboardProbe(t, probes)
	if !strings.Contains(rec.Body.String(), "does not prove Messages") || got.auth != "Bearer sk-ant-oat-manual" || got.beta != claudecode.BetaOAuth || got.url != "https://claude-manual.invalid/v1/models?limit=1000" {
		t.Fatalf("manual check %s probe %+v", rec.Body.String(), got)
	}
	if tokenHits.Load() != 0 || probeHits.Load() != 2 {
		t.Fatalf("token hits %d probe hits %d", tokenHits.Load(), probeHits.Load())
	}
	assertNoExtraProbe(t, probes)
}

func TestDashboardAuthPolicyPreservesOtherProviderBranches(t *testing.T) {
	withDashboardLocalNetwork(t)
	h := testHandler(t)
	var genericHits atomic.Int32
	probes := captureDashboardProbe(t, &genericHits)
	openai := &domain.Provider{
		Name: "openai-saved", BaseURL: "https://generic-models.invalid/v1", Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			AccessToken: "stored-generic", RefreshToken: "rt-generic", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			ClaudeScopes: "user:inference", ClaudeCodeAuth: true,
		},
	}
	if err := h.store.CreateProvider(context.Background(), openai); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"auth_method": {"oauth"}, "protocol": {string(domain.ProtocolOpenAI)},
		"base_url": {openai.BaseURL}, "id": {strconv.FormatInt(openai.ID, 10)},
	}
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	got := takeDashboardProbe(t, probes)
	if !strings.Contains(rec.Body.String(), "1 models") || got.method != http.MethodGet || got.url != "https://generic-models.invalid/v1/models" || got.auth != "Bearer stored-generic" || got.beta != "" {
		t.Fatalf("generic check %s probe %+v", rec.Body.String(), got)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/models?provider_id="+strconv.FormatInt(openai.ID, 10), nil)
	rec = httptest.NewRecorder()
	h.providerModels(rec, req)
	got = takeDashboardProbe(t, probes)
	if !strings.Contains(rec.Body.String(), `value="model"`) || got.url != "https://generic-models.invalid/v1/models" || got.auth != "Bearer stored-generic" {
		t.Fatalf("generic autocomplete %s probe %+v", rec.Body.String(), got)
	}

	var catalogHits atomic.Int32
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		catalogHits.Add(1)
		if req.Header.Get("Authorization") != "Bearer kiro-stored" {
			t.Errorf("kiro auth = %q", req.Header.Get("Authorization"))
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"Kiro-Model"}]}`), nil
	})
	kiro := &domain.Provider{
		Name: "kiro-saved", BaseURL: "https://kiro-dashboard.invalid/root/", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			KiroAuth: "idc", AccessToken: "kiro-stored", RefreshToken: "kiro-refresh", ProfileArn: "arn:synthetic",
			ExpiresAt: time.Now().Add(time.Hour).Unix(), ClaudeScopes: "user:inference",
		},
	}
	if err := h.store.CreateProvider(context.Background(), kiro); err != nil {
		t.Fatal(err)
	}
	form = url.Values{
		"auth_method": {"oauth"}, "protocol": {string(domain.ProtocolKiro)},
		"base_url": {kiro.BaseURL}, "id": {strconv.FormatInt(kiro.ID, 10)},
	}
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "catalog access confirmed") || catalogHits.Load() != 1 {
		t.Fatalf("kiro check %s hits %d", rec.Body.String(), catalogHits.Load())
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/providers/models?provider_id="+strconv.FormatInt(kiro.ID, 10), nil)
	rec = httptest.NewRecorder()
	h.providerModels(rec, req)
	if !strings.Contains(rec.Body.String(), `value="Kiro-Model"`) || catalogHits.Load() != 1 {
		t.Fatalf("kiro autocomplete %s hits %d", rec.Body.String(), catalogHits.Load())
	}

	var cursorCheckHits, cursorModelHits atomic.Int32
	cursorResponse := opencodeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != cursor.AgentBaseURL+cursor.ModelsPath {
			t.Errorf("blocked unexpected Cursor request: %s %s", req.Method, req.URL.Redacted())
			return nil, fmt.Errorf("unexpected Cursor test endpoint")
		}
		if req.Header.Get("Authorization") != "Bearer cursor-stored" {
			t.Errorf("cursor auth = %q", req.Header.Get("Authorization"))
		}
		// GetUsableModelsResponse field 1 wraps ModelDetails field 1 (id).
		const modelID = "cursor-fixture-model"
		payload := string([]byte{0x0a, byte(len(modelID) + 2), 0x0a, byte(len(modelID))}) + modelID
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), ContentLength: int64(len(payload))}, nil
	})
	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		cursorCheckHits.Add(1)
		return cursorResponse.RoundTrip(req)
	})
	// Cursor autocomplete creates its own client with the default transport.
	// Guard both paths so a static fallback cannot hide an unmocked request.
	previous := http.DefaultTransport
	http.DefaultTransport = opencodeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		cursorModelHits.Add(1)
		return cursorResponse.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	cur := &domain.Provider{
		Name: "cursor-saved", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth: true, AccessToken: "cursor-stored", RefreshToken: "cursor-session-jwt", MachineID: strings.Repeat("a", 64),
			ExpiresAt: time.Now().Add(time.Hour).Unix(), ClaudeScopes: "user:inference",
		},
	}
	if err := h.store.CreateProvider(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	form = url.Values{
		"auth_method": {"oauth"}, "protocol": {string(domain.ProtocolCursor)},
		"base_url": {cur.BaseURL}, "id": {strconv.FormatInt(cur.ID, 10)},
	}
	rec = httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "token accepted (1 models)") || cursorCheckHits.Load() != 1 || cursorModelHits.Load() != 0 {
		t.Fatalf("cursor check %s check hits %d model hits %d", rec.Body.String(), cursorCheckHits.Load(), cursorModelHits.Load())
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/providers/models?provider_id="+strconv.FormatInt(cur.ID, 10), nil)
	rec = httptest.NewRecorder()
	h.providerModels(rec, req)
	if !strings.Contains(rec.Body.String(), `value="cursor-fixture-model"`) || strings.Contains(rec.Body.String(), `value="composer-2.5"`) || strings.Contains(rec.Body.String(), "enter id manually") || cursorCheckHits.Load() != 1 || cursorModelHits.Load() != 1 {
		t.Fatalf("cursor autocomplete %s check hits %d model hits %d", rec.Body.String(), cursorCheckHits.Load(), cursorModelHits.Load())
	}
	if genericHits.Load() != 2 {
		t.Fatalf("generic probes = %d", genericHits.Load())
	}
	assertNoExtraProbe(t, probes)
}

func TestClaudeCodeDashboardCheckDoesNotRetryAuthFailure(t *testing.T) {
	withDashboardLocalNetwork(t)
	h := testHandler(t)
	var tokenHits atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits.Add(1)
		http.Error(w, "check must not refresh after rejection", http.StatusBadGateway)
	}))
	t.Cleanup(tokenSrv.Close)
	var probeHits atomic.Int32
	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		probeHits.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"denied"}`)), ContentLength: 18}, nil
	})
	p := &domain.Provider{
		Name: "claude-rejected", BaseURL: "https://claude-rejected.invalid/v1", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			ClaudeCodeAuth: true, AccessToken: "opaque-live", RefreshToken: "rt-old", ClientID: "cid-synthetic",
			ClaudeScopes: "user:inference", ClaudeDeviceID: strings.Repeat("ef", 32),
			ExpiresAt: time.Now().Add(time.Hour).Unix(), TokenURL: tokenSrv.URL,
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"auth_method": {"oauth"}, "protocol": {string(domain.ProtocolClaudeCode)},
		"base_url": {p.BaseURL}, "id": {strconv.FormatInt(p.ID, 10)},
	}
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "token rejected") || tokenHits.Load() != 0 || probeHits.Load() != 1 {
		t.Fatalf("result %s token %d probes %d", rec.Body.String(), tokenHits.Load(), probeHits.Load())
	}
}

func TestClaudeCodeModelFallbackRemainsStatic(t *testing.T) {
	h := testHandler(t)
	var hits atomic.Int32
	models, probes := newClaudeModelsServer(t, &hits, http.StatusForbidden, `{"error":"denied"}`)
	withDashboardLocalNetwork(t, models.URL+"/models?limit=1000")
	p := &domain.Provider{
		Name: "claude-fallback", BaseURL: models.URL, Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			ClaudeCodeAuth: true, AccessToken: "opaque-live", ClaudeScopes: "user:profile",
			ClaudeDeviceID: strings.Repeat("11", 32), ExpiresAt: time.Now().Add(time.Hour).Unix(),
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/models?provider_id="+strconv.FormatInt(p.ID, 10), nil)
	rec := httptest.NewRecorder()
	h.providerModels(rec, req)
	body := rec.Body.String()
	got := takeDashboardProbe(t, probes)
	if strings.Contains(body, "enter id manually") || !strings.Contains(body, `value="claude-haiku-5-5"`) || !strings.Contains(body, `value="claude-sonnet-5"`) {
		t.Fatalf("fallback = %s", body)
	}
	if hits.Load() != 1 || got.auth != "Bearer opaque-live" || got.beta != "" || got.helper {
		t.Fatalf("fallback probe = %+v hits %d", got, hits.Load())
	}
	assertNoExtraProbe(t, probes)
}

type presetConnector struct {
	creds *domain.OAuthCreds
}

func (c presetConnector) State() string { return "preset" }
func (c presetConnector) Result() (*domain.OAuthCreds, error, bool) {
	return c.creds, nil, true
}
func (c presetConnector) Close() error { return nil }
