package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
)

func savedKiroConfig(t *testing.T, h *Handler, method domain.AuthMethod) *domain.Provider {
	t.Helper()
	p := &domain.Provider{
		Name: "kiro", BaseURL: "https://stored.example", Protocol: domain.ProtocolKiro,
		AuthMethod: method, APIKey: "key",
		OAuthCreds: &domain.OAuthCreds{
			AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			ClientID: "old-client", ClientSecret: "old-secret", KiroAuth: "builder-id",
			KiroIDP: "BuilderId", ProfileArn: "arn:old", Region: "us-east-1",
			KiroTransport: "runtime", KiroDiscovery: "management", KiroAgentMode: "spec", KiroContentOptOut: true,
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func kiroEditForm(p *domain.Provider) url.Values {
	return url.Values{
		"id": {strconv.FormatInt(p.ID, 10)}, "name": {p.Name}, "protocol": {"kiro"},
		"auth_method": {string(p.Method())}, "base_url": {p.BaseURL}, "preset": {"kiro"},
	}
}

func saveKiroEdit(t *testing.T, h *Handler, p *domain.Provider, form url.Values) *domain.OAuthCreds {
	t.Helper()
	r := reqWithForm(form)
	r.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	w := httptest.NewRecorder()
	h.updateProvider(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", w.Code, w.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.OAuthCreds
}

func TestKiroAccountFieldsExplicitClear(t *testing.T) {
	for _, method := range []domain.AuthMethod{domain.AuthOAuth, domain.AuthAPIKey} {
		t.Run(string(method), func(t *testing.T) {
			h := testHandler(t)
			p := savedKiroConfig(t, h, method)
			form := kiroEditForm(p)
			for _, field := range []string{"profile_arn", "region", "kiro_auth", "kiro_idp"} {
				form.Set(field, "")
			}
			c := saveKiroEdit(t, h, p, form)
			if c.ProfileArn != "" || c.Region != "" || c.KiroAuth != "" || c.KiroIDP != "" {
				t.Fatal("explicit account clears were ignored")
			}
			if c.KiroTransport != "runtime" || !c.KiroContentOptOut {
				t.Fatal("omitted connection preferences were lost")
			}
		})
	}
}

func TestKiroConnectedIdentityConsistentAcrossCreateCheckSave(t *testing.T) {
	for _, editing := range []bool{false, true} {
		for _, profile := range []string{"arn:new", ""} {
			t.Run("edit="+strconv.FormatBool(editing)+"/profile="+profile, func(t *testing.T) {
				h := testHandler(t)
				p := &domain.Provider{Name: "new", BaseURL: "https://stored.example", AuthMethod: domain.AuthOAuth}
				if editing {
					p = savedKiroConfig(t, h, domain.AuthOAuth)
				}
				c := &domain.OAuthCreds{AccessToken: "new", RefreshToken: "new-refresh", KiroAuth: "idc", ProfileArn: profile, Region: "eu-central-1"}
				if profile != "" {
					c.KiroIDP = "AWSIdC"
				}
				before := *c
				state := putKiroSession(h, c)
				form := kiroEditForm(p)
				form.Set("oauth_session", state)
				form.Set("profile_arn", "arn:stale")
				form.Set("region", "us-west-2")
				form.Set("kiro_auth", "builder-id")
				form.Set("kiro_idp", "BuilderId")
				form.Set("kiro_transport", "codewhisperer")
				form.Set("kiro_discovery", "legacy")
				form.Set("kiro_agent_mode", "")
				form.Set("kiro_content_opt_out", "false")
				withKiroCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body["profileArn"] != profile || r.Header.Get("Authorization") != "Bearer new" || r.Header.Get("X-Kiro-Idp") != c.KiroIDP {
						t.Fatal("Check used stale account identity")
					}
					return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"live"}]}`), nil
				})
				w := httptest.NewRecorder()
				h.checkProvider(w, reqWithForm(form))
				if !strings.Contains(w.Body.String(), "1 models") {
					t.Fatalf("check=%s", w.Body.String())
				}
				var got *domain.OAuthCreds
				if editing {
					got = saveKiroEdit(t, h, p, form)
				} else {
					w = httptest.NewRecorder()
					h.createProvider(w, reqWithForm(form))
					ps, err := h.store.ListProviders(context.Background())
					if err != nil || len(ps) != 1 {
						t.Fatalf("create status=%d providers=%d err=%v", w.Code, len(ps), err)
					}
					got = ps[0].OAuthCreds
				}
				if got.ProfileArn != profile || got.Region != before.Region || got.KiroAuth != before.KiroAuth || got.KiroIDP != before.KiroIDP || got.AccessToken != "new" {
					t.Fatal("Save used stale account identity")
				}
				if got.KiroTransport != "" || got.KiroDiscovery != "" || got.KiroAgentMode != "" || got.KiroContentOptOut {
					t.Fatal("connected config resets were ignored")
				}
				if !reflect.DeepEqual(*c, before) {
					t.Fatal("connect session was mutated")
				}
			})
		}
	}
}

func TestKiroManualImportAccountPolicy(t *testing.T) {
	for _, mode := range []string{"new-account", "new-profile", "same-refresh", "refresh-only", "access-only"} {
		t.Run(mode, func(t *testing.T) {
			h := testHandler(t)
			p := savedKiroConfig(t, h, domain.AuthOAuth)
			form := kiroEditForm(p)
			form.Set("access_token", "new-access")
			form.Set("refresh_token", "new-refresh")
			same := mode == "same-refresh" || mode == "refresh-only" || mode == "access-only"
			wantProfile, wantIDP, wantAccess := "", "", "new-access"
			if same {
				form.Set("refresh_token", "old-refresh")
				wantProfile, wantIDP = "arn:old", "BuilderId"
				if mode == "refresh-only" {
					form.Del("access_token")
					wantAccess = "old"
				}
				if mode == "access-only" {
					form.Set("access_token", "old")
					form.Del("refresh_token")
					wantAccess = "old"
				}
			} else {
				form.Set("kiro_auth", "social")
				if mode == "new-profile" {
					form.Set("profile_arn", "arn:new")
					wantProfile = "arn:new"
				}
			}
			withKiroCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["profileArn"] != wantProfile || r.Header.Get("X-Kiro-Idp") != wantIDP || r.Header.Get("Authorization") != "Bearer "+wantAccess {
					t.Fatal("import Check mixed accounts")
				}
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"live"}]}`), nil
			})
			w := httptest.NewRecorder()
			h.checkProvider(w, reqWithForm(form))
			if !strings.Contains(w.Body.String(), "1 models") {
				t.Fatalf("check=%s", w.Body.String())
			}
			c := saveKiroEdit(t, h, p, form)
			if c.ProfileArn != wantProfile || c.KiroIDP != wantIDP || c.AccessToken != wantAccess {
				t.Fatal("import Save mixed accounts")
			}
			if same && (c.ClientID != "old-client" || c.ClientSecret != "old-secret" || c.RefreshToken != "old-refresh") {
				t.Fatal("same-account refresh registration was lost")
			}
			if !same && (c.ClientID != "" || c.ClientSecret != "" || c.Region != "") {
				t.Fatal("new import inherited old registration")
			}
			if c.KiroTransport != "runtime" || c.KiroDiscovery != "management" || !c.KiroContentOptOut {
				t.Fatal("omitted import preferences were lost")
			}
		})
	}
}

type kiroUnavailableStore struct {
	updated *domain.OAuthCreds
}

func (*kiroUnavailableStore) GetProvider(context.Context, int64) (*domain.Provider, error) {
	return nil, errors.New("store unavailable")
}

func (s *kiroUnavailableStore) UpdateProviderOAuth(_ context.Context, _ int64, c *domain.OAuthCreds) error {
	s.updated = c
	return nil
}

func TestKiroProbePreservesCompleteCredentialsOnStoreFailure(t *testing.T) {
	c := &domain.OAuthCreds{
		AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1, KiroAuth: "builder-id", Region: "us-east-1",
		ClientID: "cid", ClientSecret: "secret", TokenURL: "https://token.example", Email: "account@example.com",
		AccountID: "account", IDToken: "identity", ExtraAuthParams: map[string]string{"key": "value"},
	}
	r := reqWithForm(url.Values{})
	_ = r.ParseForm()
	got := kiroProbeCreds(r, &domain.Provider{OAuthCreds: c})
	if !reflect.DeepEqual(got, c) {
		t.Fatal("probe dropped credentials")
	}
	got.ExtraAuthParams["key"] = "changed"
	if c.ExtraAuthParams["key"] != "value" {
		t.Fatal("probe modified source credentials")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["clientId"] != "cid" || body["clientSecret"] != "secret" || body["refreshToken"] != "refresh" {
			t.Error("snapshot refresh registration was lost")
		}
		_, _ = io.WriteString(w, `{"accessToken":"fresh","expiresIn":3600}`)
	}))
	defer server.Close()
	withKiroTokenTransport(t, server)
	st := &kiroUnavailableStore{}
	token, err := oauth.New(st).Resolve(context.Background(), &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: got}, false)
	if err != nil || token != "fresh" || st.updated == nil || st.updated.ClientID != "cid" {
		t.Fatalf("snapshot refresh token=%q err=%v", token, err)
	}
}

func TestKiroManualRefreshPreservesAndResetsConfig(t *testing.T) {
	for _, mode := range []string{"omitted", "reset", "new-account"} {
		t.Run(mode, func(t *testing.T) {
			h := testHandler(t)
			p := savedKiroConfig(t, h, domain.AuthOAuth)
			form := kiroEditForm(p)
			wantClient, wantRefresh := "old-client", "old-refresh"
			if mode == "reset" {
				for _, field := range []string{"profile_arn", "region", "kiro_idp", "kiro_agent_mode", "kiro_transport", "kiro_discovery"} {
					form.Set(field, "")
				}
				form.Set("kiro_content_opt_out", "false")
			}
			if mode == "new-account" {
				form.Set("refresh_token", "new-refresh")
				form.Set("client_id", "new-client")
				form.Set("client_secret", "new-secret")
				form.Set("kiro_auth", "builder-id")
				wantClient, wantRefresh = "new-client", "new-refresh"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["clientId"] != wantClient || body["refreshToken"] != wantRefresh {
					t.Error("refresh used wrong account registration")
				}
				_, _ = io.WriteString(w, `{"accessToken":"rotated","refreshToken":"rotated-refresh","expiresIn":3600}`)
			}))
			defer server.Close()
			withKiroTokenTransport(t, server)
			w := httptest.NewRecorder()
			h.oauthRefreshTokens(w, reqWithForm(form))
			got, err := h.store.GetProvider(context.Background(), p.ID)
			if err != nil || got.OAuthCreds.AccessToken != "rotated" {
				t.Fatalf("refresh failed: %v result=%s", err, w.Body.String())
			}
			c := got.OAuthCreds
			if mode == "reset" {
				if c.ProfileArn != "" || c.Region != "" || c.KiroIDP != "" || c.KiroTransport != "" || c.KiroDiscovery != "" || c.KiroAgentMode != "" || c.KiroContentOptOut {
					t.Fatal("refresh ignored explicit clears")
				}
			} else {
				if c.KiroTransport != "runtime" || c.KiroDiscovery != "management" || !c.KiroContentOptOut {
					t.Fatal("refresh lost omitted preferences")
				}
				if mode == "new-account" && (c.ProfileArn != "" || c.KiroIDP != "" || c.Region != "" || c.ClientID != "new-client") {
					t.Fatal("refresh inherited previous account identity")
				}
			}
		})
	}
}
