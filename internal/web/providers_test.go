package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func TestProviderRecipeFormsIncludeTagsField(t *testing.T) {
	for _, r := range recipes {
		html := renderComponent(t, ProviderRecipeForm(r))
		if !strings.Contains(html, `name="tags"`) {
			t.Errorf("recipe %s missing tags field", r.ID)
		}
	}
}

func TestProviderEditRowsIncludeTagsField(t *testing.T) {
	generic := &domain.Provider{ID: 1, Name: "p", BaseURL: "https://x", Protocol: domain.ProtocolOpenAI, Tags: []string{"prod"}}
	if html := renderComponent(t, providerEditRowGenericAPIKey(generic)); !strings.Contains(html, `name="tags"`) || !strings.Contains(html, `value="prod"`) {
		t.Fatalf("generic edit tags: %s", html)
	}
	opencodeP := &domain.Provider{ID: 2, Name: "p", BaseURL: "https://opencode.ai/zen/v1", Protocol: domain.ProtocolOpencode, Tags: []string{"eu"}}
	if html := renderComponent(t, providerEditRowOpencode(opencodeP)); !strings.Contains(html, `name="tags"`) {
		t.Fatalf("opencode edit tags: %s", html)
	}
	kiro := &domain.Provider{ID: 3, Name: "k", BaseURL: "https://x", Protocol: domain.ProtocolKiro}
	if html := renderComponent(t, providerEditRowKiro(kiro)); !strings.Contains(html, `name="tags"`) {
		t.Fatalf("kiro edit tags: %s", html)
	}
	qoder := &domain.Provider{ID: 4, Name: "q", BaseURL: "https://x", Protocol: domain.ProtocolQoder, AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{}}
	if html := renderComponent(t, providerEditRowQoder(qoder)); !strings.Contains(html, `name="tags"`) {
		t.Fatalf("qoder edit tags: %s", html)
	}
	cursor := &domain.Provider{ID: 5, Name: "c", BaseURL: "https://x", Protocol: domain.ProtocolCursor, AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{}}
	if html := renderComponent(t, providerEditRowCursor(cursor)); !strings.Contains(html, `name="tags"`) {
		t.Fatalf("cursor edit tags: %s", html)
	}
	oauth := &domain.Provider{ID: 6, Name: "o", BaseURL: "https://x", Protocol: domain.ProtocolOpenAICodex, AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{}}
	if html := renderComponent(t, providerEditRowInteractiveOAuth(oauth)); !strings.Contains(html, `name="tags"`) {
		t.Fatalf("interactive oauth edit tags: %s", html)
	}
}

func TestProviderListTagBadgesAndFilterMetadata(t *testing.T) {
	ps := []*domain.Provider{
		{ID: 1, Name: "tagged", BaseURL: "https://a", Protocol: domain.ProtocolOpenAI, Tags: []string{"eu", "prod"}},
		{ID: 2, Name: "plain", BaseURL: "https://b", Protocol: domain.ProtocolOpenAI},
		{ID: 3, Name: "old", BaseURL: "https://c", Protocol: domain.ProtocolOpenAI, Tags: []string{"prod"}, Archived: true},
	}
	html := renderComponent(t, ProviderList(ps))
	if !strings.Contains(html, `class="provider-tag">eu</span>`) || !strings.Contains(html, `class="provider-tag">prod</span>`) {
		t.Fatalf("missing badges: %s", html)
	}
	if strings.Contains(html, `class="provider-tag">Untagged</span>`) {
		t.Fatalf("must not show Untagged badge: %s", html)
	}
	if !strings.Contains(html, `data-tags="eu,prod"`) || !strings.Contains(html, `data-tags=""`) {
		t.Fatalf("missing row tag metadata: %s", html)
	}
	if !strings.Contains(html, `id="provider-tag-filter"`) {
		t.Fatalf("missing filter: %s", html)
	}
	if !strings.Contains(html, `data-tag-filter="eu"`) || !strings.Contains(html, `data-tag-filter="prod"`) {
		t.Fatalf("missing tag buttons: %s", html)
	}
	if !strings.Contains(html, `data-tag-filter="__untagged__"`) {
		t.Fatalf("missing Untagged control: %s", html)
	}
	if !strings.Contains(html, `id="provider-filter-empty"`) {
		t.Fatalf("missing no-match message: %s", html)
	}
}

func TestCreateProviderParsesTags(t *testing.T) {
	h := testHandler(t)
	form := url.Values{
		"name":        {"p1"},
		"base_url":    {"https://x"},
		"api_key":     {"k"},
		"protocol":    {"openai"},
		"auth_method": {"apikey"},
		"tags":        {" Beta, alpha, Alpha "},
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	list, err := h.store.ListProviders(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v err=%v", list, err)
	}
	if got := strings.Join(list[0].Tags, ","); got != "alpha,beta" {
		t.Fatalf("tags = %v", list[0].Tags)
	}
}

func TestCreateProviderRejectsInvalidTags(t *testing.T) {
	h := testHandler(t)
	form := url.Values{
		"name":        {"p1"},
		"base_url":    {"https://x"},
		"api_key":     {"k"},
		"protocol":    {"openai"},
		"auth_method": {"apikey"},
		"tags":        {"foo--bar"},
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	list, err := h.store.ListProviders(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("should not persist invalid tags: %v err=%v", list, err)
	}
}

func TestUpdateProviderReplacesTags(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{Name: "p1", BaseURL: "https://x", APIKey: "k", Protocol: domain.ProtocolOpenAI, Tags: []string{"old"}}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"name":        {"p1"},
		"base_url":    {"https://x"},
		"protocol":    {"openai"},
		"auth_method": {"apikey"},
		"auth_scheme": {"bearer"},
		"tags":        {""},
	}
	path := "/dashboard/providers/" + strconv.FormatInt(p.ID, 10)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("cleared tags = %v", got.Tags)
	}
}
