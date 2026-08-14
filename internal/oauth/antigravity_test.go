package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
)

func TestAntigravityAuthorizeURL(t *testing.T) {
	p, ok := PresetByName("antigravity")
	if !ok {
		t.Fatal("missing preset")
	}
	_, creds := Apply(p)
	c, err := NewConnect(creds)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.AuthorizeURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "access_type=offline") || !strings.Contains(u, "prompt=consent") {
		t.Fatalf("extras missing: %s", u)
	}
	if strings.Contains(u, "code_challenge") {
		t.Fatal("should not use PKCE")
	}
	if !creds.AntigravityAuth {
		t.Fatal("marker")
	}
}

type codeAssistCapture struct {
	userAgent      string
	apiClient      string
	clientMetadata string
	body           []byte
}

func captureCodeAssist(r *http.Request) codeAssistCapture {
	body, _ := io.ReadAll(r.Body)
	return codeAssistCapture{
		userAgent:      r.Header.Get("User-Agent"),
		apiClient:      r.Header.Get("X-Goog-Api-Client"),
		clientMetadata: r.Header.Get("Client-Metadata"),
		body:           body,
	}
}

func (c codeAssistCapture) assertHeaders(t *testing.T) {
	t.Helper()
	if c.apiClient != "" {
		t.Error("X-Goog-Api-Client must not be sent")
	}
	if c.clientMetadata != "" {
		t.Error("Client-Metadata must not be sent")
	}
	if c.userAgent != antigravity.UserAgent {
		t.Errorf("User-Agent = %q, want %q", c.userAgent, antigravity.UserAgent)
	}
}

func (c codeAssistCapture) assertBodyMetadata(t *testing.T) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(c.body, &payload); err != nil {
		t.Fatal(err)
	}
	meta, ok := payload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("missing body metadata: %s", c.body)
	}
	if meta["ideType"] != float64(9) || meta["platform"] != float64(3) || meta["pluginType"] != float64(2) {
		t.Fatalf("metadata = %#v", meta)
	}
}

func withAntigravityEndpoints(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldLoad, oldOnboard, oldInfo := agLoadCodeAssistURL, agOnboardUserURL, agUserInfoURL
	agLoadCodeAssistURL = srv.URL + "/v1internal:loadCodeAssist"
	agOnboardUserURL = srv.URL + "/v1internal:onboardUser"
	agUserInfoURL = srv.URL + "/oauth2/v1/userinfo?alt=json"
	t.Cleanup(func() {
		agLoadCodeAssistURL, agOnboardUserURL, agUserInfoURL = oldLoad, oldOnboard, oldInfo
	})
}

func TestFinalizeAntigravity(t *testing.T) {
	var loadCap, onboardCap codeAssistCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "userinfo"):
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "a@b.com", "name": "A"})
		case strings.Contains(r.URL.String(), "loadCodeAssist"):
			loadCap = captureCodeAssist(r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cloudaicompanionProject": "proj-xyz",
				"allowedTiers":            []any{map[string]any{"id": "free-tier", "isDefault": true}},
			})
		case strings.Contains(r.URL.String(), "onboardUser"):
			onboardCap = captureCodeAssist(r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true,
				"response": map[string]any{
					"cloudaicompanionProject": map[string]string{"id": "proj-final"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withAntigravityEndpoints(t, srv)

	c := &domain.OAuthCreds{AntigravityAuth: true, AccessToken: "tok"}
	if err := finalizeAntigravity(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	loadCap.assertHeaders(t)
	loadCap.assertBodyMetadata(t)
	onboardCap.assertHeaders(t)
	onboardCap.assertBodyMetadata(t)
	if c.ProjectID != "proj-final" {
		t.Fatalf("project %q", c.ProjectID)
	}
	if c.Email != "a@b.com" {
		t.Fatalf("email %q", c.Email)
	}
}

func TestFinalizeAntigravityFreshAccountOnboards(t *testing.T) {
	var loadCap, onboardCap codeAssistCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "userinfo"):
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "fresh@b.com"})
		case strings.Contains(r.URL.String(), "loadCodeAssist"):
			loadCap = captureCodeAssist(r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"allowedTiers": []any{map[string]any{"id": "free-tier", "isDefault": true}},
			})
		case strings.Contains(r.URL.String(), "onboardUser"):
			onboardCap = captureCodeAssist(r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true,
				"response": map[string]any{
					"cloudaicompanionProject": "proj-onboarded",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withAntigravityEndpoints(t, srv)

	c := &domain.OAuthCreds{AntigravityAuth: true, AccessToken: "tok"}
	if err := finalizeAntigravity(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	loadCap.assertHeaders(t)
	onboardCap.assertHeaders(t)
	if c.ProjectID != "proj-onboarded" {
		t.Fatalf("project %q", c.ProjectID)
	}
}

func TestFinalizeAntigravityNoProjectAfterOnboarding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "userinfo"):
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "a@b.com"})
		case strings.Contains(r.URL.String(), "loadCodeAssist"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"allowedTiers": []any{map[string]any{"id": "free-tier", "isDefault": true}},
			})
		case strings.Contains(r.URL.String(), "onboardUser"):
			_ = json.NewEncoder(w).Encode(map[string]any{"done": true, "response": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withAntigravityEndpoints(t, srv)

	c := &domain.OAuthCreds{AntigravityAuth: true, AccessToken: "tok"}
	err := finalizeAntigravity(context.Background(), c)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `no project id after onboarding (tier "free-tier")`) {
		t.Fatalf("error = %q", err)
	}
	if c.ProjectID != "" {
		t.Fatalf("project %q", c.ProjectID)
	}
}

func TestFinalizeAntigravityUsesCurrentTierID(t *testing.T) {
	var gotTier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "userinfo"):
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "a@b.com"})
		case strings.Contains(r.URL.String(), "loadCodeAssist"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"currentTierId": "paid-tier",
				"allowedTiers":  []any{map[string]any{"id": "free-tier", "isDefault": true}},
			})
		case strings.Contains(r.URL.String(), "onboardUser"):
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			if id, _ := payload["tierId"].(string); id != "" {
				gotTier = id
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true,
				"response": map[string]any{
					"cloudaicompanionProject": "proj-tier",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withAntigravityEndpoints(t, srv)

	c := &domain.OAuthCreds{AntigravityAuth: true, AccessToken: "tok"}
	if err := finalizeAntigravity(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if gotTier != "paid-tier" {
		t.Fatalf("onboard tierId = %q, want paid-tier", gotTier)
	}
	if c.ProjectID != "proj-tier" {
		t.Fatalf("project %q", c.ProjectID)
	}
}

func TestEnsureAntigravityProjectSkipsWhenSet(t *testing.T) {
	c := &domain.OAuthCreds{AntigravityAuth: true, ProjectID: "already"}
	if err := EnsureAntigravityProject(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.ProjectID != "already" {
		t.Fatal(c.ProjectID)
	}
}

func TestApplyAntigravityPreset(t *testing.T) {
	p, ok := PresetByName("antigravity")
	if !ok {
		t.Fatal("missing")
	}
	prov, creds := Apply(p)
	if prov.Protocol != domain.ProtocolAntigravity {
		t.Fatal(prov.Protocol)
	}
	if !creds.AntigravityAuth || creds.ClientSecret == "" || creds.PKCE {
		t.Fatalf("%+v", creds)
	}
	if !strings.Contains(creds.Scopes, "cloud-platform") {
		t.Fatal(creds.Scopes)
	}
}

func TestExtractProjectID(t *testing.T) {
	if got := extractProjectID("abc"); got != "abc" {
		t.Fatal(got)
	}
	if got := extractProjectID(map[string]any{"id": " z "}); got != "z" {
		t.Fatal(got)
	}
}

func TestTruncateBody(t *testing.T) {
	t.Run("short unchanged", func(t *testing.T) {
		s := "short body"
		if got := truncateBody([]byte(s)); got != s {
			t.Errorf("got %q, want %q", got, s)
		}
	})
	t.Run("exactly at limit unchanged", func(t *testing.T) {
		s := strings.Repeat("x", 200)
		if got := truncateBody([]byte(s)); got != s {
			t.Errorf("len = %d, want %d", len(got), len(s))
		}
	})
	t.Run("over limit truncates with marker", func(t *testing.T) {
		s := strings.Repeat("x", 250)
		got := truncateBody([]byte(s))
		if !strings.HasSuffix(got, "...") {
			t.Errorf("expected ... suffix, got %q", got)
		}
		if len(got) > 203 {
			t.Errorf("len = %d, want <= 203 (200 + ...)", len(got))
		}
		if !strings.HasPrefix(got, strings.Repeat("x", 200)) {
			t.Errorf("expected first 200 bytes preserved, got %q", got)
		}
	})
	t.Run("empty unchanged", func(t *testing.T) {
		if got := truncateBody(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
