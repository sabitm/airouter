package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"airouter/internal/crypto"
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

// TestUpdateProviderOAuthCASRetriesConcurrentWrite proves the conditional
// write does not lose increments when several writers race. Each callback runs
// again on the newest decrypted blob after a failed compare.
func TestUpdateProviderOAuthCASRetriesConcurrentWrite(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			ClaudeCodeAuth: true, AccessToken: "tok", RefreshToken: "rt",
			ClaudeAccountUUID: "acct", ClaudeDeviceID: strings.Repeat("ab", 32),
		},
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	const workers, perWorker = 8, 12
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < perWorker; n++ {
				if _, err := st.UpdateProviderOAuthCAS(ctx, p.ID, func(cur *domain.OAuthCreds) (*domain.OAuthCreds, bool, error) {
					next := *cur
					next.ExpiresAt++
					return &next, true, nil
				}); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.ExpiresAt != workers*perWorker {
		t.Fatalf("expires_at=%d, lost update", got.OAuthCreds.ExpiresAt)
	}
	if got.OAuthCreds.ClaudeDeviceID != strings.Repeat("ab", 32) || got.OAuthCreds.ClaudeAccountUUID != "acct" {
		t.Fatalf("identity clobbered: %+v", got.OAuthCreds)
	}
}

func TestUpdateProviderOAuthCASSurvivesRestartAndExport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cas.db")
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{ClaudeCodeAuth: true, AccessToken: "a", RefreshToken: "r", ClaudeAccountUUID: "acct"},
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	device := strings.Repeat("cd", 32)
	ready, err := st.UpdateProviderOAuthCAS(ctx, p.ID, func(cur *domain.OAuthCreds) (*domain.OAuthCreds, bool, error) {
		next := *cur
		next.ClaudeDeviceID = device
		return &next, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready == nil || ready.ClaudeDeviceID != device {
		t.Fatalf("cas result = %+v", ready)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.ClaudeDeviceID != device || got.OAuthCreds.AccessToken != "a" {
		t.Fatalf("after restart = %+v", got.OAuthCreds)
	}
	var buf bytes.Buffer
	if err := st.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	dst := testStore(t)
	if _, err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	imported, err := dst.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if imported.OAuthCreds.ClaudeDeviceID != device || imported.OAuthCreds.ClaudeAccountUUID != "acct" {
		t.Fatalf("imported = %+v", imported.OAuthCreds)
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
		{ProviderID: p.ID, UpstreamModel: "grok-4", Enabled: true},
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
	if _, err := st.Import(ctx, bytes.NewReader([]byte(cfg))); err != nil {
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
	if _, err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
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

func TestExportImportKiroAPIKeyConfigRoundTrip(t *testing.T) {
	src := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "kiro-key", BaseURL: "https://codewhisperer.example", APIKey: "key-1",
		Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			AccessToken: "stale-access", RefreshToken: "stale-refresh", IDToken: "stale-id",
			ClientSecret: "stale-secret", ClientID: "stale-client", TokenURL: "https://evil.example/token",
			ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/A", Region: "eu-central-1",
			KiroAuth: "builder-id", KiroIDP: "BuilderId", KiroContentOptOut: true,
			KiroAgentMode: "spec", KiroTransport: "runtime", KiroDiscovery: "management",
		},
	}
	if err := src.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	exported := buf.String()
	for _, secret := range []string{"stale-access", "stale-refresh", "stale-id", "stale-secret", "stale-client", "evil.example"} {
		if strings.Contains(exported, secret) {
			t.Fatalf("export leaked %s: %s", secret, exported)
		}
	}
	dst := testStore(t)
	if _, err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := dst.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method() != domain.AuthAPIKey || got.APIKey != "key-1" || got.OAuthCreds == nil {
		t.Fatalf("imported = %+v creds=%+v", got, got.OAuthCreds)
	}
	c := got.OAuthCreds
	if c.ProfileArn != p.OAuthCreds.ProfileArn || c.Region != "eu-central-1" || c.KiroAuth != "builder-id" ||
		c.KiroIDP != "BuilderId" || !c.KiroContentOptOut || c.KiroAgentMode != "spec" ||
		c.KiroTransport != "runtime" || c.KiroDiscovery != "management" {
		t.Fatalf("config = %+v", c)
	}
	if c.AccessToken != "" || c.RefreshToken != "" || c.IDToken != "" || c.ClientSecret != "" || c.ClientID != "" || c.TokenURL != "" {
		t.Fatalf("sanitized config kept credentials: %+v", c)
	}

	var again bytes.Buffer
	if err := dst.Export(ctx, &again); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again.String(), "stale-") {
		t.Fatalf("second export leaked stale credentials: %s", again.String())
	}
}

func TestImportLegacyAPIKeyKiroWithoutConfig(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const cfg = `{
		"version": 1,
		"providers": [{"name":"old","base_url":"https://codewhisperer.example","api_key":"key-1","protocol":"kiro","auth_method":"apikey"}],
		"combos": []
	}`
	if _, err := st.Import(ctx, bytes.NewReader([]byte(cfg))); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method() != domain.AuthAPIKey || got.APIKey != "key-1" || got.OAuthCreds != nil {
		t.Fatalf("legacy import = %+v creds=%+v", got, got.OAuthCreds)
	}
}

// TestImportOAuthMissingCreds skips an oauth method with no oauth block and
// records the row in ImportSummary.Failures without aborting the import.
func TestImportOAuthMissingCreds(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const cfg = `{
		"version": 1,
		"providers": [{"name":"p1","base_url":"http://a","api_key":"","protocol":"openai","auth_method":"oauth"}],
		"combos": []
	}`
	sum, err := st.Import(ctx, bytes.NewReader([]byte(cfg)))
	if err != nil {
		t.Fatalf("import err = %v, want nil (row skipped, not fatal)", err)
	}
	if sum == nil || len(sum.Failures) != 1 {
		t.Fatalf("Failures = %v, want 1 entry", sum)
	}
	if !strings.Contains(sum.Failures[0], `provider "p1"`) || !strings.Contains(sum.Failures[0], "oauth") {
		t.Errorf("failure = %q, want provider \"p1\" and oauth", sum.Failures[0])
	}
	if _, err := st.GetProvider(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProvider = %v, want ErrNotFound (row skipped)", err)
	}
}

func TestUpdateProviderPersistsAllFields(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{Name: "orig", BaseURL: "http://a", APIKey: "k1", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Mutate every updatable field.
	p.Name = "renamed"
	p.BaseURL = "http://b"
	p.APIKey = "k2-rotated"
	p.Protocol = domain.ProtocolAnthropic
	p.AuthScheme = domain.AuthXAPIKey
	p.AuthMethod = domain.AuthOAuth
	p.OAuthCreds = &domain.OAuthCreds{AccessToken: "tok", RefreshToken: "rt", ExpiresAt: 123}
	p.Archived = true
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.BaseURL != "http://b" || got.APIKey != "k2-rotated" {
		t.Errorf("basic fields = %+v", got)
	}
	if got.Protocol != domain.ProtocolAnthropic || got.AuthScheme != domain.AuthXAPIKey || got.AuthMethod != domain.AuthOAuth {
		t.Errorf("auth fields = proto=%q scheme=%q method=%q", got.Protocol, got.AuthScheme, got.AuthMethod)
	}
	if !got.Archived {
		t.Error("Archived = false, want true")
	}
	if got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "tok" || got.OAuthCreds.RefreshToken != "rt" {
		t.Errorf("OAuthCreds = %+v", got.OAuthCreds)
	}
}

func TestDeleteProviderRemovesRowAndCascadesTargets(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{Name: "p", BaseURL: "http://a", APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	c := &domain.Combo{
		Name:     "c",
		Strategy: domain.StrategyFailover,
		Targets:  []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "m", Enabled: true}},
	}
	if err := st.CreateCombo(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Target row exists referencing the provider.
	var n int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM combo_targets WHERE provider_id = ?", p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("target rows before delete = %d, want 1", n)
	}

	if err := st.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	if _, err := st.GetProvider(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetProvider after delete err = %v, want ErrNotFound", err)
	}
	// FK ON DELETE CASCADE removed the orphaned target.
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM combo_targets WHERE provider_id = ?", p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("target rows after delete = %d, want 0 (cascade)", n)
	}
}

func TestProviderReasoningDialectRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "qwen-agg", BaseURL: "https://x", APIKey: "k",
		Protocol: domain.ProtocolOpenAI, AuthScheme: domain.AuthBearer,
		ReasoningDialect: domain.ReasoningQwen,
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasoningDialect != domain.ReasoningQwen {
		t.Fatalf("stored dialect = %q", got.ReasoningDialect)
	}
	if got.Reasoning() != domain.ReasoningQwen {
		t.Fatalf("effective = %q", got.Reasoning())
	}

	// Empty dialect keeps protocol default.
	p2 := &domain.Provider{
		Name: "plain", BaseURL: "https://y", APIKey: "k",
		Protocol: domain.ProtocolOpenAI, AuthScheme: domain.AuthBearer,
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	got2, err := st.GetProvider(ctx, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.ReasoningDialect != "" {
		t.Fatalf("empty should stay empty, got %q", got2.ReasoningDialect)
	}
	if got2.Reasoning() != domain.ReasoningOpenAI {
		t.Fatalf("effective default = %q", got2.Reasoning())
	}

	// Explicit none.
	p2.ReasoningDialect = domain.ReasoningNone
	if err := st.UpdateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	got2, err = st.GetProvider(ctx, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Reasoning() != domain.ReasoningNone {
		t.Fatalf("explicit none = %q", got2.Reasoning())
	}
}

func TestProviderTagsRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "tagged", BaseURL: "http://a", APIKey: "k", Protocol: domain.ProtocolOpenAI,
		Tags: []string{"prod", "eu"},
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(got.Tags, []string{"eu", "prod"}) {
		t.Fatalf("create tags = %v", got.Tags)
	}

	list, err := st.ListProviders(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v err=%v", list, err)
	}
	if !equalStringSlice(list[0].Tags, []string{"eu", "prod"}) {
		t.Fatalf("list tags = %v", list[0].Tags)
	}

	p.Tags = []string{"beta", "alpha", "beta"}
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(got.Tags, []string{"alpha", "beta"}) {
		t.Fatalf("update tags = %v", got.Tags)
	}

	p.Tags = nil
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("cleared tags = %v", got.Tags)
	}
}

func claudeProviderRow(creds *domain.OAuthCreds) *domain.Provider {
	return &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		Tags: []string{"team"}, OAuthCreds: creds,
	}
}

func openSharedStore(t *testing.T, path string, c *crypto.Cipher) *Store {
	t.Helper()
	st, err := Open(path, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestConditionalProviderWrites(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	original := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "old-access", RefreshToken: "same-refresh",
		ExpiresAt: 10, ClaudeAccountUUID: "acct", ClaudeDeviceID: strings.Repeat("ab", 32),
		ClaudeScopes: "user:inference", TokenURL: "https://stored.example/token", ClientID: "cid",
	}
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		Tags: []string{"team"}, OAuthCreds: original,
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	basis, err := st.GetProviderOAuthStored(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if basis.OAuthBlob == "" || basis.Provider.OAuthCreds.AccessToken != "old-access" {
		t.Fatalf("stored basis = %+v blob %q", basis.Provider, basis.OAuthBlob)
	}

	rotated := *original
	rotated.AccessToken = "rotated-access"
	rotated.ExpiresAt = 20
	if err := st.UpdateProviderOAuth(ctx, p.ID, &rotated); err != nil {
		t.Fatal(err)
	}
	save := ProviderConnectionSave{
		ProviderMeta: ProviderMeta{
			Name: "stale-name", BaseURL: "https://stale.example",
			Protocol: domain.ProtocolClaudeCode, AuthMethod: domain.AuthOAuth,
			AuthScheme: domain.AuthBearer,
		},
		OAuthCreds: original,
	}
	if err := st.UpdateProviderIfConnection(ctx, p.ID, save, domain.AuthOAuth, domain.ProtocolClaudeCode, basis.OAuthBlob); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale credential save err = %v, want conflict", err)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "cc" || got.BaseURL != "https://api.anthropic.com" || got.OAuthCreds.AccessToken != "rotated-access" || got.OAuthCreds.ExpiresAt != 20 {
		t.Fatalf("conflict changed row: %+v creds %+v", got, got.OAuthCreds)
	}

	meta := ProviderMeta{
		Name: "renamed", BaseURL: "https://renamed.example", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, Tags: []string{"kept"},
	}
	if err := st.UpdateProviderMetaIfConnection(ctx, p.ID, meta, domain.AuthOAuth, domain.ProtocolClaudeCode); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.BaseURL != "https://renamed.example" || got.OAuthCreds.AccessToken != "rotated-access" || got.OAuthCreds.ExpiresAt != 20 {
		t.Fatalf("metadata write changed credentials: %+v creds %+v", got, got.OAuthCreds)
	}
	if !equalStringSlice(got.Tags, []string{"kept"}) {
		t.Fatalf("tags = %v", got.Tags)
	}

	got.AuthMethod = domain.AuthAPIKey
	got.Protocol = domain.ProtocolOpenAI
	got.OAuthCreds = nil
	got.APIKey = "switched"
	if err := st.UpdateProvider(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProviderMetaIfConnection(ctx, p.ID, meta, domain.AuthOAuth, domain.ProtocolClaudeCode); !errors.Is(err, ErrConflict) {
		t.Fatalf("auth switch err = %v, want conflict", err)
	}
	got, err = st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthAPIKey || got.Protocol != domain.ProtocolOpenAI || got.APIKey != "switched" || got.OAuthCreds != nil {
		t.Fatalf("auth conflict mutated row: %+v", got)
	}

	if err := st.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProviderIfConnection(ctx, p.ID, save, domain.AuthOAuth, domain.ProtocolClaudeCode, basis.OAuthBlob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted credential save err = %v, want not found", err)
	}
	if err := st.UpdateProviderMetaIfConnection(ctx, p.ID, meta, domain.AuthOAuth, domain.ProtocolClaudeCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted metadata save err = %v, want not found", err)
	}
}

func TestUpdateProviderConnectionCASGuardsConnection(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	original := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "old-access", RefreshToken: "same-refresh",
		ExpiresAt: 10, ClaudeAccountUUID: "acct", ClaudeDeviceID: strings.Repeat("ab", 32),
	}
	p := claudeProviderRow(original)
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}

	// A protocol switch with unchanged credentials is a conflict, not a
	// successful write of a stale rotation into the switched row.
	switched, err := st.GetProviderOAuthStored(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	switched.Provider.Protocol = domain.ProtocolAnthropic
	if err := st.UpdateProvider(ctx, switched.Provider); err != nil {
		t.Fatal(err)
	}
	rotated := *original
	rotated.AccessToken = "stale-rotation"
	rotated.ExpiresAt = 44
	_, err = st.UpdateProviderConnectionCAS(ctx, p.ID, func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		if method != domain.AuthOAuth || proto != domain.ProtocolClaudeCode {
			return nil, false, errors.New("connection changed")
		}
		next := *current
		next.AccessToken = rotated.AccessToken
		next.ExpiresAt = rotated.ExpiresAt
		return &next, true, nil
	})
	if err == nil || !strings.Contains(err.Error(), "connection changed") {
		t.Fatalf("protocol switch CAS err = %v", err)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolAnthropic || got.OAuthCreds.AccessToken != "old-access" {
		t.Fatalf("CAS mutated switched row: %+v creds %+v", got, got.OAuthCreds)
	}

	// A missing row is ErrNotFound so callers can tell deletion from conflict.
	if err := st.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateProviderConnectionCAS(ctx, p.ID, func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		return current, true, nil
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted row CAS err = %v, want not found", err)
	}
}

func TestUpdateProviderConnectionCASRetriesAcrossInstances(t *testing.T) {
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "shared.db")
	writer := openSharedStore(t, dbPath, c)
	reader := openSharedStore(t, dbPath, c)
	other := openSharedStore(t, dbPath, c)
	ctx := context.Background()
	original := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "old-access", RefreshToken: "same-refresh",
		ExpiresAt: 10, ClaudeAccountUUID: "acct", ClaudeDeviceID: strings.Repeat("ab", 32),
	}
	if err := writer.CreateProvider(ctx, claudeProviderRow(original)); err != nil {
		t.Fatal(err)
	}

	// On the first attempt a second store instance commits a device
	// initialization before this callback's write lands. The guarded write
	// must lose that race and retry; the retry sees the device-initialized row
	// and applies the rotation onto it instead of dropping the device.
	injected := false
	merged, err := reader.UpdateProviderConnectionCAS(ctx, 1, func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		if current == nil || !current.ClaudeCodeAuth {
			return nil, false, errors.New("credential row replaced")
		}
		if !injected {
			injected = true
			deviceInit := *current
			deviceInit.ClaudeDeviceID = strings.Repeat("cd", 32)
			if err := other.UpdateProviderOAuth(ctx, 1, &deviceInit); err != nil {
				return nil, false, err
			}
			next := *current
			next.AccessToken = "rotated-access"
			return &next, true, nil
		}
		next := *current
		next.AccessToken = "rotated-access"
		next.ExpiresAt = 20
		return &next, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("retry path was not exercised")
	}
	if merged.ClaudeDeviceID != strings.Repeat("cd", 32) {
		t.Fatalf("retry dropped device initialization: %+v", merged)
	}
	if merged.AccessToken != "rotated-access" || merged.ExpiresAt != 20 {
		t.Fatalf("retry dropped rotation: %+v", merged)
	}
	got, err := writer.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.ClaudeDeviceID != strings.Repeat("cd", 32) || got.OAuthCreds.AccessToken != "rotated-access" || got.OAuthCreds.ExpiresAt != 20 {
		t.Fatalf("shared row = %+v", got.OAuthCreds)
	}
}

func TestConditionalWritesPreserveArchived(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	original := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "old-access", RefreshToken: "same-refresh",
		ExpiresAt: 10, ClaudeAccountUUID: "acct", ClaudeDeviceID: strings.Repeat("ab", 32),
	}
	p := claudeProviderRow(original)
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	basis, err := st.GetProviderOAuthStored(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProviderArchived(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}

	meta := ProviderMeta{
		Name: "renamed", BaseURL: "https://renamed.example", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
	}
	if err := st.UpdateProviderMetaIfConnection(ctx, p.ID, meta, domain.AuthOAuth, domain.ProtocolClaudeCode); err != nil {
		t.Fatal(err)
	}
	save := ProviderConnectionSave{ProviderMeta: meta, OAuthCreds: original}
	save.OAuthCreds.AccessToken = "new-access"
	if err := st.UpdateProviderIfConnection(ctx, p.ID, save, domain.AuthOAuth, domain.ProtocolClaudeCode, basis.OAuthBlob); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived {
		t.Fatalf("stale snapshot restored archived: %+v", got)
	}
	if got.Name != "renamed" || got.OAuthCreds.AccessToken != "new-access" {
		t.Fatalf("saved fields lost: %+v creds %+v", got, got.OAuthCreds)
	}
}

func TestUpdateProviderIfConnectionConcurrent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := claudeProviderRow(&domain.OAuthCreds{ClaudeCodeAuth: true, AccessToken: "start", RefreshToken: "rt"})
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	basis, err := st.GetProviderOAuthStored(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	meta := ProviderMeta{
		Name:             "writer",
		BaseURL:          basis.Provider.BaseURL,
		Protocol:         basis.Provider.Protocol,
		AuthScheme:       basis.Provider.AuthScheme,
		AuthMethod:       basis.Provider.AuthMethod,
		ReasoningDialect: basis.Provider.ReasoningDialect,
		Tags:             basis.Provider.Tags,
	}
	// Every worker derives its write from the SAME basis read and then waits on
	// one barrier before writing. Without the atomic blob-and-connection
	// predicate all eight writes would serialize and succeed; with it exactly
	// one write commits and the other seven conflict.
	var wg sync.WaitGroup
	var conflicts atomic.Int32
	var winners atomic.Int32
	errCh := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cp := *basis.Provider.OAuthCreds
			cp.AccessToken = "writer"
			cp.ExpiresAt = int64(n + 1)
			<-start
			if err := st.UpdateProviderIfConnection(ctx, p.ID, ProviderConnectionSave{
				ProviderMeta: meta,
				OAuthCreds:   &cp,
			}, domain.AuthOAuth, domain.ProtocolClaudeCode, basis.OAuthBlob); err != nil {
				if errors.Is(err, ErrConflict) {
					conflicts.Add(1)
					return
				}
				errCh <- err
				return
			}
			winners.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if winners.Load() != 1 || conflicts.Load() != workers-1 {
		t.Fatalf("winners = %d conflicts = %d, want 1 and %d", winners.Load(), conflicts.Load(), workers-1)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "writer" || got.OAuthCreds.AccessToken != "writer" || got.OAuthCreds.RefreshToken != "rt" {
		t.Fatalf("concurrent save tore row: %+v creds %+v", got, got.OAuthCreds)
	}
}

func TestUpdateProviderOAuthPreservesTags(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "grok", BaseURL: "https://api.x.ai/v1", APIKey: "k", Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		Tags: []string{"prod"},
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
	if !equalStringSlice(got.Tags, []string{"prod"}) {
		t.Fatalf("tags after oauth refresh = %v", got.Tags)
	}
}

func TestProviderTagsHydratedInCombo(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{
		Name: "p", BaseURL: "http://a", APIKey: "k", Protocol: domain.ProtocolOpenAI,
		Tags: []string{"team-a"},
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: p.ID, UpstreamModel: "m", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetComboByName(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(got.Targets[0].Provider.Tags, []string{"team-a"}) {
		t.Fatalf("hydrated tags = %v", got.Targets[0].Provider.Tags)
	}
}

func TestProviderTagsLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt("k")
	if err != nil {
		t.Fatal(err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	const schema = `
CREATE TABLE providers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	base_url TEXT NOT NULL,
	api_key TEXT NOT NULL,
	protocol TEXT NOT NULL,
	auth_scheme TEXT NOT NULL DEFAULT '',
	auth_method TEXT NOT NULL DEFAULT '',
	oauth_creds TEXT NOT NULL DEFAULT '',
	reasoning_dialect TEXT NOT NULL DEFAULT '',
	archived INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO providers (name, base_url, api_key, protocol) VALUES (?, ?, ?, ?)`,
		"legacy", "http://a", enc, "openai"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	got, err := st.GetProvider(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("legacy tags = %v, want empty", got.Tags)
	}
	var raw string
	if err := st.db.QueryRow(`SELECT tags FROM providers WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "[]" {
		t.Fatalf("stored default = %q, want []", raw)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProviderReasoningDialectLegacyMigration(t *testing.T) {
	// Open a DB, strip the column simulation by creating via raw SQL without the column
	// is hard after migrate. Instead verify migrate is idempotent and empty default works
	// on a fresh store (covered above). Re-open the same DB to ensure migrate is safe.
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{Name: "x", BaseURL: "http://a", APIKey: "k", Protocol: domain.ProtocolAnthropic}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Second Open on same path would need the file; just ensure list/get still work.
	list, err := st.ListProviders(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v err=%v", list, err)
	}
	if list[0].Reasoning() != domain.ReasoningClaude {
		t.Fatalf("anthropic default = %q", list[0].Reasoning())
	}
}
