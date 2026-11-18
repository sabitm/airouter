package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/opencode"
)

func TestUsagePageRegistersOpenCodeGoConsoleOnly(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	must := func(p *domain.Provider) {
		t.Helper()
		if err := h.store.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	console := openCodeUsageProvider("go-console", opencode.GoBaseURL, domain.AuthOAuth, "")
	console.Tags = []string{"eu"}
	console.OAuthCreds = openCodeUsageCreds("org-1", "Workspace", "access-secret", "refresh-secret")
	must(console)
	must(&domain.Provider{
		Name: "zen-live", BaseURL: opencode.ZenBaseURL, Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthAPIKey, APIKey: opencode.PublicKey, Tags: []string{"eu"},
	})
	must(&domain.Provider{
		Name: "go-key", BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthAPIKey, APIKey: "go-secret",
	})
	mirror := openCodeUsageProvider("go-mirror", "https://mirror.example/inference/go/v1", domain.AuthOAuth, "")
	mirror.OAuthCreds = openCodeUsageCreds("org-1", "Workspace", "access-secret", "refresh-secret")
	must(mirror)
	archived := openCodeUsageProvider("go-archived", opencode.GoBaseURL, domain.AuthOAuth, "")
	archived.Archived = true
	archived.OAuthCreds = openCodeUsageCreds("org-1", "Workspace", "access-secret", "refresh-secret")
	must(archived)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/usage?tag=eu", nil)
	h.usagePage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "go-console") || !strings.Contains(body, ">Load</button>") {
		t.Fatalf("console card missing: %s", body)
	}
	for _, absent := range []string{"zen-live", "go-key", "go-mirror", "go-archived", "access-secret", "org-1"} {
		if strings.Contains(body, absent) {
			t.Fatalf("page contains %s: %s", absent, body)
		}
	}
	if strings.Contains(body, "hx-trigger=\"load\"") {
		t.Fatalf("idle page autoloaded: %s", body)
	}
}

func TestUsageCardRendersOpenCodeGoQuota(t *testing.T) {
	var hits int
	restore := oauth.SetOpencodeConsoleClientForTest(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("console token host called: %s", r.URL.Host)
		return nil, io.ErrUnexpectedEOF
	})})
	t.Cleanup(restore)
	h := testHandler(t)
	h.usage.SetClientForTest(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		if r.URL.String() != opencode.GoUsageURL {
			t.Errorf("url = %s", r.URL.String())
		}
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer access-secret" || r.Header.Get("x-opencode-org-id") != "org-secret" {
			t.Errorf("method=%s auth=%q org=%q", r.Method, r.Header.Get("Authorization"), r.Header.Get("x-opencode-org-id"))
		}
		reset := time.Now().Add(26 * time.Hour).UTC().Format(time.RFC3339)
		// 27.4 used is 72.6 remaining. The card rounds that to 73.
		pct := 27.4
		if hits > 1 {
			pct = 10
		}
		body := `{"usage":{"monthly":{"status":"ok","percent":0,"resetsAt":"` + reset + `"},"weekly":{"status":"rate-limited","percent":10,"resetsAt":"` + reset + `"},"rolling":{"status":"ok","percent":` + strconv.FormatFloat(pct, 'f', 1, 64) + `,"resetsAt":"` + reset + `"}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})})
	p := openCodeUsageProvider("go-console", opencode.GoBaseURL, domain.AuthOAuth, "")
	p.OAuthCreds = openCodeUsageCreds("org-secret", "Workspace", "access-secret", "refresh-secret")
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	body := getUsageCard(t, h, p.ID, false)
	for _, want := range []string{"5-hour", "Weekly", "Monthly", "73% remaining", "0% remaining", "100% remaining", "Rate-limited: Weekly.", "OpenCode Go", "?force=1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("card missing %q: %s", want, body)
		}
	}
	five := strings.Index(body, `quota-name">5-hour`)
	week := strings.Index(body, `quota-name">Weekly`)
	month := strings.Index(body, `quota-name">Monthly`)
	if five < 0 || week < 0 || month < 0 || five > week || week > month {
		t.Fatalf("window order wrong: %s", body)
	}
	if strings.Contains(body, "0/0") || strings.Contains(body, `<span class="quota-reset">-</span>`) {
		t.Fatalf("card fabricated units or reset: %s", body)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "org-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("card contains %s", secret)
		}
	}
	forced := getUsageCard(t, h, p.ID, true)
	if hits != 2 || !strings.Contains(forced, "90% remaining") {
		t.Fatalf("force did not bypass cache hits=%d body=%s", hits, forced)
	}
}

func openCodeUsageProvider(name, base string, method domain.AuthMethod, key string) *domain.Provider {
	return &domain.Provider{
		Name: name, BaseURL: base, Protocol: domain.ProtocolOpencode,
		AuthMethod: method, AuthScheme: domain.AuthBearer, APIKey: key,
	}
}

func openCodeUsageCreds(org, name, access, refresh string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		AccessToken:         access,
		RefreshToken:        refresh,
		ExpiresAt:           2_000_000_000,
		AccountID:           "user-1",
		OpencodeConsoleAuth: true,
		OpencodeOrgID:       org,
		OpencodeOrgName:     name,
		ClientID:            opencode.ConsoleClientID,
	}
}

func getUsageCard(t *testing.T, h *Handler, id int64, force bool) string {
	t.Helper()
	sid := strconv.FormatInt(id, 10)
	u := "/dashboard/usage/card/" + sid
	if force {
		u += "?force=1"
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.SetPathValue("id", sid)
	h.usageCard(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
