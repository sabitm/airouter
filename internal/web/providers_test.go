package web

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"airouter/internal/domain"

	"github.com/a-h/templ"
)

func renderComponent(t *testing.T, comp templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := comp.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestProviderEditRowGenericAPIKeyProtocolEditable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proto   domain.Protocol
		wantSel []string
	}{
		{"openai", domain.ProtocolOpenAI, []string{"openai", "openai-responses", "anthropic"}},
		{"openai-responses", domain.ProtocolOpenAIResponses, []string{"openai", "openai-responses", "anthropic"}},
		{"anthropic", domain.ProtocolAnthropic, []string{"openai", "openai-responses", "anthropic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &domain.Provider{ID: 1, Name: "p", BaseURL: "https://x", Protocol: tc.proto}
			html := renderComponent(t, providerEditRowGenericAPIKey(p))
			if !strings.Contains(html, `<select name="protocol"`) {
				t.Fatalf("want protocol select; html=%s", html)
			}
			if strings.Contains(html, `type="hidden" name="protocol"`) {
				t.Fatalf("want no hidden protocol input for editable proto; html=%s", html)
			}
			for _, opt := range tc.wantSel {
				if !strings.Contains(html, `value="`+opt+`"`) {
					t.Fatalf("missing option %q; html=%s", opt, html)
				}
			}
			// selected attribute is rendered via templ boolean attribute as ` selected`
			if !strings.Contains(html, `value="`+tc.name+`" selected`) {
				t.Fatalf("want %q selected; html=%s", tc.name, html)
			}
		})
	}
}

func TestProviderEditRowGenericAPIKeyProtocolLockedForSpecific(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto domain.Protocol
	}{
		{"kiro", domain.ProtocolKiro},
		{"qoder", domain.ProtocolQoder},
		{"cursor", domain.ProtocolCursor},
		{"antigravity", domain.ProtocolAntigravity},
		{"codex", domain.ProtocolOpenAICodex},
		{"claude-code", domain.ProtocolClaudeCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &domain.Provider{ID: 2, Name: "p", BaseURL: "https://x", Protocol: tc.proto, AuthMethod: domain.AuthAPIKey}
			html := renderComponent(t, providerEditRowGenericAPIKey(p))
			if !strings.Contains(html, `type="hidden" name="protocol" value="`+string(tc.proto)+`"`) {
				t.Fatalf("want hidden locked protocol=%q; html=%s", tc.proto, html)
			}
			if strings.Contains(html, `<select name="protocol"`) {
				t.Fatalf("want no protocol select for %q; html=%s", tc.proto, html)
			}
		})
	}
}

func TestProviderEditRowReasoningDialectSelector(t *testing.T) {
	p := &domain.Provider{ID: 1, Name: "p", BaseURL: "https://x", Protocol: domain.ProtocolOpenAI}
	html := renderComponent(t, providerEditRowGenericAPIKey(p))
	if !strings.Contains(html, `name="reasoning_dialect"`) {
		t.Fatalf("want dialect select; html=%s", html)
	}
	for _, opt := range []string{"none", "openai", "kimi", "qwen", "deepseek", "zai", "grok"} {
		if !strings.Contains(html, `value="`+opt+`"`) {
			t.Fatalf("missing dialect option %q; html=%s", opt, html)
		}
	}
	// Anthropic: only none/claude
	p.Protocol = domain.ProtocolAnthropic
	html = renderComponent(t, providerEditRowGenericAPIKey(p))
	if !strings.Contains(html, `value="claude"`) {
		t.Fatalf("want claude option; html=%s", html)
	}
	if strings.Contains(html, `value="qwen"`) {
		t.Fatalf("anthropic should not offer qwen; html=%s", html)
	}
}

func TestCursorRecipeDefaultsToWebAuth(t *testing.T) {
	r, ok := recipeByID("cursor")
	if !ok {
		t.Fatal("missing cursor recipe")
	}
	if r.Tag != "OAuth" || r.Kind != kindCursor {
		t.Fatalf("recipe = %+v", r)
	}
	html := renderComponent(t, ProviderRecipeForm(r))
	if !strings.Contains(html, `data-oauth-mode="web"`) {
		t.Fatalf("want web oauth mode; html=%s", html)
	}
	if !strings.Contains(html, `/dashboard/providers/cursor/begin`) {
		t.Fatalf("want cursor begin route; html=%s", html)
	}
	if !strings.Contains(html, `name="refresh_token"`) {
		t.Fatalf("want manual refresh token field; html=%s", html)
	}
}

func TestCursorEditRowReconnectAndRefresh(t *testing.T) {
	p := &domain.Provider{
		ID: 9, Name: "c", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{CursorAuth: true, RefreshToken: "rt", MachineID: "mid"},
	}
	html := renderComponent(t, providerEditRowCursor(p))
	if strings.Contains(html, "cannot be refreshed") {
		t.Fatalf("connected text still claims no refresh: %s", html)
	}
	if !strings.Contains(html, `/dashboard/providers/cursor/begin`) {
		t.Fatalf("want reconnect begin; html=%s", html)
	}
	if !strings.Contains(html, "Refresh token") {
		t.Fatalf("want refresh action; html=%s", html)
	}
}

func TestCursorEditRowSessionJWTHidesRefresh(t *testing.T) {
	header := "eyJhbGciOiJub25lIn0"
	payload := "eyJpc3MiOiJodHRwczovL2F1dGhlbnRpY2F0aW9uLmN1cnNvci5zaCJ9"
	sess := header + "." + payload + "."
	p := &domain.Provider{
		ID: 9, Name: "c", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{CursorAuth: true, RefreshToken: sess, MachineID: "mid"},
	}
	html := renderComponent(t, providerEditRowCursor(p))
	if strings.Contains(html, "oauth-refresh-result-") {
		t.Fatalf("session JWT should hide connected refresh: %s", html)
	}
	if !strings.Contains(html, "cannot be rotated") {
		t.Fatalf("want session-cannot-rotate hint; html=%s", html)
	}
}

func TestGrokRecipeDefaultsToGrokDialect(t *testing.T) {
	r, ok := recipeByID("xai")
	if !ok {
		t.Fatal("missing xai recipe")
	}
	html := renderComponent(t, ProviderRecipeForm(r))
	if !strings.Contains(html, `name="reasoning_dialect"`) || !strings.Contains(html, `value="grok" selected`) {
		t.Fatalf("grok recipe should preselect grok dialect; html=%s", html)
	}
}

func TestProviderEditRowReasoningDialectLocked(t *testing.T) {
	p := &domain.Provider{ID: 2, Name: "p", BaseURL: "https://x", Protocol: domain.ProtocolOpenAICodex, AuthMethod: domain.AuthOAuth}
	html := renderComponent(t, providerEditRowInteractiveOAuth(p))
	if !strings.Contains(html, `type="hidden" name="reasoning_dialect" value="codex"`) {
		t.Fatalf("want locked codex dialect; html=%s", html)
	}
}
