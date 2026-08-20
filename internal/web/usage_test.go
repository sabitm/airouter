package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/usage"
)

func TestUsagePageListsSupportedNonArchived(t *testing.T) {
	h := testHandler(t)
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()

	mustCreate := func(p *domain.Provider) {
		t.Helper()
		if err := h.store.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	mustCreate(&domain.Provider{
		Name: "codex-live", Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "t"},
	})
	mustCreate(&domain.Provider{
		Name: "claude-live", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "t"},
	})
	mustCreate(&domain.Provider{
		Name: "openai-plain", Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthAPIKey, APIKey: "k",
	})
	mustCreate(&domain.Provider{
		Name: "codex-archived", Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "t"},
		Archived:   true,
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/usage", nil)
	h.usagePage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "codex-live") || !strings.Contains(body, "claude-live") {
		t.Fatalf("missing supported cards: %s", body)
	}
	if strings.Contains(body, "openai-plain") {
		t.Fatalf("plain openai should be excluded: %s", body)
	}
	if strings.Contains(body, "codex-archived") {
		t.Fatalf("archived should be excluded: %s", body)
	}
	if !strings.Contains(body, "hx-get=\"/dashboard/usage?load=1\"") {
		t.Fatalf("missing Load all control: %s", body)
	}
	if strings.Count(body, ">Load</button>") != 2 {
		t.Fatalf("each idle card should have a Load button: %s", body)
	}
	if !strings.Contains(body, "hx-get=\"/dashboard/usage/card/") {
		t.Fatalf("missing per-provider Load controls: %s", body)
	}
	if strings.Contains(body, "hx-trigger=\"load\"") {
		t.Fatalf("page must not autoload on navigation: %s", body)
	}
	if strings.Contains(body, "loading...") {
		t.Fatalf("idle cards must not show loading skeletons: %s", body)
	}
}

func TestUsageCardFilledAndForceBypassesCache(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 20},
			},
		})
	}))
	t.Cleanup(up.Close)
	prev := usage.CodexUsageURL
	usage.CodexUsageURL = up.URL
	t.Cleanup(func() { usage.CodexUsageURL = prev })

	h := testHandler(t)
	p := &domain.Provider{
		Name: "codex-live", Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "tok", ExpiresAt: 0},
	}
	if err := h.store.CreateProvider(httptest.NewRequest(http.MethodGet, "/", nil).Context(), p); err != nil {
		t.Fatal(err)
	}

	getCard := func(force bool) string {
		t.Helper()
		id := strconv.FormatInt(p.ID, 10)
		u := "/dashboard/usage/card/" + id
		if force {
			u += "?force=1"
		}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.SetPathValue("id", id)
		h.usageCard(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	body := getCard(false)
	if !strings.Contains(body, "codex-live") || !strings.Contains(body, "plus") {
		t.Fatalf("filled card: %s", body)
	}
	if !strings.Contains(body, "session (5h)") {
		t.Fatalf("missing quota row: %s", body)
	}
	if hits != 1 {
		t.Fatalf("hits = %d", hits)
	}
	_ = getCard(false)
	if hits != 1 {
		t.Fatalf("cached hits = %d", hits)
	}
	_ = getCard(true)
	if hits != 2 {
		t.Fatalf("force hits = %d", hits)
	}
}

func TestUsageCardUnsupported404(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{Name: "oai", Protocol: domain.ProtocolOpenAI, APIKey: "k"}
	if err := h.store.CreateProvider(httptest.NewRequest(http.MethodGet, "/", nil).Context(), p); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	id := strconv.FormatInt(p.ID, 10)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/usage/card/"+id, nil)
	req.SetPathValue("id", id)
	h.usageCard(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestUsageLoadAllPartialAutoloads(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "codex-live", Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{AccessToken: "t"},
	}
	if err := h.store.CreateProvider(httptest.NewRequest(http.MethodGet, "/", nil).Context(), p); err != nil {
		t.Fatal(err)
	}

	// Full navigation carrying ?load=1 must still render the idle page, not an
	// autoloading grid, so a stray query parameter cannot trigger upstream calls.
	full := httptest.NewRecorder()
	fullReq := httptest.NewRequest(http.MethodGet, "/dashboard/usage?load=1", nil)
	h.usagePage(full, fullReq)
	if !strings.Contains(full.Body.String(), "<html") {
		t.Fatalf("full navigation with ?load=1 should render the page: %s", full.Body.String())
	}
	if strings.Contains(full.Body.String(), "hx-trigger=\"load\"") {
		t.Fatalf("full navigation must not autoload: %s", full.Body.String())
	}

	// The explicit Load all HTMX request returns a grid partial whose skeletons
	// autoload, using cached (non-forced) card requests.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/usage?load=1", nil)
	req.Header.Set("HX-Request", "true")
	h.usagePage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("load-all should be a grid partial: %s", body)
	}
	if !strings.Contains(body, "hx-trigger=\"load\"") {
		t.Fatalf("load-all cards should autoload: %s", body)
	}
	if !strings.Contains(body, "loading...") {
		t.Fatalf("load-all should render skeletons: %s", body)
	}
	if !strings.Contains(body, "hx-get=\"/dashboard/usage/card/") {
		t.Fatalf("load-all skeletons should request card endpoints: %s", body)
	}
	if strings.Contains(body, "force=1") {
		t.Fatalf("load-all should use cached data, not force refresh: %s", body)
	}
}

func TestFormatResetCountdown(t *testing.T) {
	if formatResetCountdown(nil) != "-" {
		t.Fatal("nil")
	}
	past := time.Now().Add(-time.Minute)
	if formatResetCountdown(&past) != "-" {
		t.Fatal("past")
	}
	mins := time.Now().Add(40 * time.Minute)
	if got := formatResetCountdown(&mins); got != "40m" {
		t.Fatalf("40m = %q", got)
	}
	hours := time.Now().Add(2*time.Hour + 5*time.Minute)
	if got := formatResetCountdown(&hours); got != "2h 5m" {
		t.Fatalf("2h 5m = %q", got)
	}
	days := time.Now().Add(50 * time.Hour)
	if got := formatResetCountdown(&days); got != "2d 2h" {
		t.Fatalf("2d 2h = %q", got)
	}
}
