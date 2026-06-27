package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"airouter/internal/domain"
)

func TestOAuthProviderRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	p := &domain.Provider{
		Name:       "grok",
		BaseURL:    "https://api.x.ai/v1",
		APIKey:     "", // oauth providers carry no static key
		Protocol:   domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth,
		AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode:         domain.OAuthAuto,
			Preset:       "xai",
			AccessToken:  "eyJ-access",
			RefreshToken: "rt-refresh",
			ExpiresAt:    1800000000,
			Email:        "u@example.com",
			IDToken:      "eyJ-id",
		},
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method() != domain.AuthOAuth {
		t.Errorf("method = %q, want oauth", got.Method())
	}
	if got.Auth() != domain.AuthBearer {
		t.Errorf("auth = %q, want bearer", got.Auth())
	}
	if got.OAuthCreds == nil {
		t.Fatal("oauth creds nil after reload")
	}
	if got.OAuthCreds.AccessToken != "eyJ-access" || got.OAuthCreds.RefreshToken != "rt-refresh" {
		t.Errorf("tokens = %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.Preset != "xai" || got.OAuthCreds.Email != "u@example.com" {
		t.Errorf("preset/email = %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.ExpiresAt != 1800000000 {
		t.Errorf("expires_at = %d", got.OAuthCreds.ExpiresAt)
	}
	if got.APIKey != "" {
		t.Errorf("apikey should be empty for oauth, got %q", got.APIKey)
	}
}

// TestUpdateProviderOAuth refreshes only the oauth_creds column, leaving the
// provider's other fields intact.
func TestUpdateProviderOAuth(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	p := &domain.Provider{
		Name: "grok", BaseURL: "https://api.x.ai/v1", APIKey: "k", Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{Mode: domain.OAuthAuto, Preset: "xai",
			AccessToken: "old-access", RefreshToken: "rt", ExpiresAt: 100},
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}

	rotated := &domain.OAuthCreds{Mode: domain.OAuthAuto, Preset: "xai",
		AccessToken: "new-access", RefreshToken: "rt2", ExpiresAt: 200}
	if err := st.UpdateProviderOAuth(ctx, p.ID, rotated); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.AccessToken != "new-access" || got.OAuthCreds.RefreshToken != "rt2" {
		t.Errorf("rotated creds = %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.ExpiresAt != 200 {
		t.Errorf("expires_at = %d, want 200", got.OAuthCreds.ExpiresAt)
	}
	// Non-oauth fields must survive the targeted update.
	if got.Name != "grok" || got.BaseURL != "https://api.x.ai/v1" || got.Protocol != domain.ProtocolOpenAI {
		t.Errorf("provider identity changed: %+v", got)
	}
}

// TestOAuthProviderHydratedInCombo verifies the hot path decrypts oauth creds
// onto a combo target's provider, the path the proxy resolves tokens through.
func TestOAuthProviderHydratedInCombo(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "grok", BaseURL: "https://api.x.ai/v1", APIKey: "", Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{Mode: domain.OAuthAuto, Preset: "xai",
			AccessToken: "tok", RefreshToken: "rt", ExpiresAt: 1},
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: p.ID, UpstreamModel: "grok-4"},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetComboByName(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	hp := got.Targets[0].Provider
	if hp.Method() != domain.AuthOAuth || hp.OAuthCreds == nil || hp.OAuthCreds.AccessToken != "tok" {
		t.Errorf("hydrated oauth provider = %+v", hp)
	}
}

// TestImportLegacyAuthMethod confirms exports written before OAuth (no
// auth_method field) still import as apikey providers.
func TestImportLegacyAuthMethod(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const cfg = `{
		"version": 1,
		"providers": [{"name":"p1","base_url":"http://a","api_key":"k1","protocol":"openai"}],
		"combos": []
	}`
	if err := st.Import(ctx, bytes.NewReader([]byte(cfg))); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method() != domain.AuthAPIKey {
		t.Errorf("method = %q, want apikey for legacy import", got.Method())
	}
	if got.OAuthCreds != nil {
		t.Errorf("oauth creds should be nil for apikey provider")
	}
}

// TestExportImportOAuthRoundTrip confirms an oauth provider (with plaintext
// tokens) survives export + import into a fresh store.
func TestExportImportOAuthRoundTrip(t *testing.T) {
	src := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "grok", BaseURL: "https://api.x.ai/v1", APIKey: "", Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{Mode: domain.OAuthManual, AccessToken: "a", RefreshToken: "r",
			ExpiresAt: 99, TokenURL: "https://auth.x.ai/oauth2/token", ClientID: "cid", Scopes: "s"},
	}
	if err := src.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	// Exported tokens are plaintext; the blob must contain them unencrypted.
	var raw struct {
		Providers []struct {
			AuthMethod string             `json:"auth_method"`
			OAuth      *domain.OAuthCreds `json:"oauth"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Providers) != 1 || raw.Providers[0].AuthMethod != "oauth" {
		t.Fatalf("exported provider = %+v", raw.Providers)
	}
	if raw.Providers[0].OAuth == nil || raw.Providers[0].OAuth.AccessToken != "a" {
		t.Errorf("exported oauth creds = %+v", raw.Providers[0].OAuth)
	}

	dst := testStore(t)
	if err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := dst.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method() != domain.AuthOAuth || got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "a" {
		t.Errorf("imported oauth provider = %+v", got)
	}
	if got.OAuthCreds.RefreshToken != "r" || got.OAuthCreds.ClientID != "cid" {
		t.Errorf("imported oauth config = %+v", got.OAuthCreds)
	}
}

// TestImportOAuthMissingCreds rejects an oauth method with no oauth block.
func TestImportOAuthMissingCreds(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const cfg = `{
		"version": 1,
		"providers": [{"name":"p1","base_url":"http://a","api_key":"","protocol":"openai","auth_method":"oauth"}],
		"combos": []
	}`
	err := st.Import(ctx, bytes.NewReader([]byte(cfg)))
	if err == nil {
		t.Fatal("import succeeded, want error for oauth without creds")
	}
}
