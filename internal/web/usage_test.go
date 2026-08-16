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
	if !strings.Contains(body, "loading...") {
		t.Fatalf("page should render skeletons, not wait on upstream: %s", body)
	}
	if strings.Contains(body, "hx-get=\"/dashboard/usage/card/") == false {
		t.Fatalf("missing lazy card loads: %s", body)
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

func TestUsageRefreshAllPartial(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "codex-live", Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{AccessToken: "t"},
	}
	if err := h.store.CreateProvider(httptest.NewRequest(http.MethodGet, "/", nil).Context(), p); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/usage?refresh=1", nil)
	req.Header.Set("HX-Request", "true")
	h.usagePage(rr, req)
	body := rr.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("refresh-all should be a grid partial: %s", body)
	}
	if !strings.Contains(body, "force=1") {
		t.Fatalf("refresh-all cards should force: %s", body)
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
