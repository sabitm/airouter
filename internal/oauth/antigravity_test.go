package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
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

func TestFinalizeAntigravity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "userinfo"):
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "a@b.com", "name": "A"})
		case strings.Contains(r.URL.RawQuery, "loadCodeAssist") || strings.HasSuffix(r.URL.Path, "loadCodeAssist") || strings.Contains(r.URL.Path, "loadCodeAssist"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cloudaicompanionProject": "proj-xyz",
				"allowedTiers":            []any{map[string]any{"id": "free-tier", "isDefault": true}},
			})
		case strings.Contains(r.URL.Path, "onboardUser"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true,
				"response": map[string]any{
					"cloudaicompanionProject": map[string]string{"id": "proj-final"},
				},
			})
		default:
			// Google paths use colon RPC style on path.
			if strings.Contains(r.URL.String(), "loadCodeAssist") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"cloudaicompanionProject": "proj-xyz",
					"allowedTiers":            []any{map[string]any{"id": "free-tier", "isDefault": true}},
				})
				return
			}
			if strings.Contains(r.URL.String(), "onboardUser") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"done": true,
					"response": map[string]any{
						"cloudaicompanionProject": map[string]string{"id": "proj-final"},
					},
				})
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldLoad, oldOnboard, oldInfo := agLoadCodeAssistURL, agOnboardUserURL, agUserInfoURL
	agLoadCodeAssistURL = srv.URL + "/v1internal:loadCodeAssist"
	agOnboardUserURL = srv.URL + "/v1internal:onboardUser"
	agUserInfoURL = srv.URL + "/oauth2/v1/userinfo?alt=json"
	defer func() {
		agLoadCodeAssistURL, agOnboardUserURL, agUserInfoURL = oldLoad, oldOnboard, oldInfo
	}()

	c := &domain.OAuthCreds{AntigravityAuth: true, AccessToken: "tok"}
	if err := finalizeAntigravity(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.ProjectID != "proj-final" {
		t.Fatalf("project %q", c.ProjectID)
	}
	if c.Email != "a@b.com" {
		t.Fatalf("email %q", c.Email)
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
