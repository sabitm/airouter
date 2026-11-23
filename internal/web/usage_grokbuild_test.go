package web

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
)

func TestUsagePageAndCardRenderGrokBuild(t *testing.T) {
	var hits int
	grokbuild.SetTestTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		if r.Method != http.MethodGet || r.URL.String() != grokbuild.BillingURL {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer access-secret" || r.Header.Get("X-Userid") != "user-1" || r.Header.Get("X-Email") != "" {
			t.Errorf("headers = %v", r.Header)
		}
		if strings.Contains(r.Header.Get("Authorization"), "refresh-secret") {
			t.Errorf("refresh token sent")
		}
		body := `{"subscriptionTier":"SuperGrok Heavy","config":{"creditUsagePercent":42.5,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-06-08T00:00:00Z"},"prepaidBalance":{"val":1250},"onDemandCap":{"val":5000},"onDemandUsed":{"val":0}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	h := testHandler(t)
	ctx := context.Background()
	build := grokBuildDashboardProvider("build-live", grokBuildDashboardCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	build.Tags = []string{"lab"}
	if err := h.store.CreateProvider(ctx, build); err != nil {
		t.Fatal(err)
	}
	archived := grokBuildDashboardProvider("build-archived", grokBuildDashboardCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	archived.Archived = true
	if err := h.store.CreateProvider(ctx, archived); err != nil {
		t.Fatal(err)
	}
	invalid := grokBuildDashboardProvider("build-invalid", grokBuildDashboardCreds("conn-1", "user-1", "access-secret", "refresh-secret"))
	invalid.BaseURL = "https://api.x.ai/v1"
	if err := h.store.CreateProvider(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	presetOnly := grokBuildDashboardProvider("build-preset", &domain.OAuthCreds{Preset: grokbuild.Preset, AccessToken: "access-secret", RefreshToken: "refresh-secret"})
	if err := h.store.CreateProvider(ctx, presetOnly); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateProvider(ctx, &domain.Provider{
		Name: "grok-live", Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{Preset: "xai", AccessToken: "xai-secret"},
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/usage?tag=lab", nil)
	h.usagePage(rr, req)
	page := rr.Body.String()
	if !strings.Contains(page, "build-live") || !strings.Contains(page, ">Load</button>") || !strings.Contains(page, "grok-build") {
		t.Fatalf("missing Grok Build card: %s", page)
	}
	for _, absent := range []string{"build-archived", "build-invalid", "build-preset", "grok-live", "access-secret", "refresh-secret"} {
		if strings.Contains(page, absent) {
			t.Fatalf("page contains %s: %s", absent, page)
		}
	}

	card := getUsageCard(t, h, build.ID, false)
	for _, want := range []string{"Weekly", "42.5/100", "Prepaid balance: $12.50", "On-demand: $0.00 spent of $50.00", "SuperGrok Heavy", `title="2026-06-08T00:00:00Z"`, "?force=1"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q: %s", want, card)
		}
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "secret@example.com", "0/0"} {
		if strings.Contains(card, secret) {
			t.Fatalf("card contains %s: %s", secret, card)
		}
	}
	if strings.Contains(card, "Monthly included") || strings.Contains(card, "Reset usage") {
		t.Fatalf("card invented a quota or control: %s", card)
	}
	forced := getUsageCard(t, h, build.ID, true)
	if hits != 2 || !strings.Contains(forced, "42.5/100") {
		t.Fatalf("force did not bypass cache hits=%d body=%s", hits, forced)
	}

	bad := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodGet, "/dashboard/usage/card/"+itoa(invalid.ID), nil)
	badReq.SetPathValue("id", itoa(invalid.ID))
	h.usageCard(bad, badReq)
	if bad.Code != http.StatusNotFound {
		t.Fatalf("invalid card status = %d", bad.Code)
	}
}

func TestUsageCardDropsGrokBuildReportAfterReconnect(t *testing.T) {
	started := make(chan struct{})
	var startOnce sync.Once
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	grokbuild.SetTestTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "cli-chat-proxy.grok.com" {
			t.Errorf("unexpected host %s", r.URL.Host)
			return nil, io.ErrUnexpectedEOF
		}
		startOnce.Do(func() { close(started) })
		<-releaseCh
		body := `{"subscriptionTier":"OldTierMarker","config":{"creditUsagePercent":10,"prepaidBalance":{"val":7777}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	h := testHandler(t)
	p := grokBuildDashboardProvider("build-live", grokBuildDashboardCreds("conn-old", "user-1", "access-old", "refresh-old"))
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		rr := httptest.NewRecorder()
		sid := itoa(p.ID)
		req := httptest.NewRequest(http.MethodGet, "/dashboard/usage/card/"+sid, nil)
		req.SetPathValue("id", sid)
		h.usageCard(rr, req)
		done <- rr.Body.String()
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("billing request did not start")
	}
	next := grokBuildDashboardCreds("conn-new", "user-1", "access-new", "refresh-new")
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, next); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	select {
	case body := <-done:
		for _, stale := range []string{"OldTierMarker", "$77.77", "access-old", "refresh-old"} {
			if strings.Contains(body, stale) {
				t.Fatalf("stale report rendered %s: %s", stale, body)
			}
		}
		if !strings.Contains(body, "Grok Build connection changed. Reload and retry.") {
			t.Fatalf("missing conflict message: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage card did not finish")
	}
}

func grokBuildDashboardProvider(name string, creds *domain.OAuthCreds) *domain.Provider {
	return &domain.Provider{
		Name: name, BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: creds,
	}
}

func grokBuildDashboardCreds(conn, user, access, refresh string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, Preset: grokbuild.Preset, GrokBuildAuth: true,
		GrokBuildConnection: conn, UserID: user, AccessToken: access, RefreshToken: refresh,
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(), Email: "secret@example.com",
		ClientID: grokbuild.ClientID, TokenURL: grokbuild.TokenURL, AuthURL: grokbuild.Issuer,
		Scopes: grokbuild.Scopes,
	}
}

func itoa(n int64) string {
	return strconvFormat(n)
}

func strconvFormat(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type grokBuildWireCall struct {
	method  string
	rawURL  string
	auth    string
	userID  string
	refresh string
	client  string
}

func TestUsageCardRendersLegacyGrokBuildRefresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expiry int64
	}{
		{"expired", time.Now().Add(-time.Hour).Unix()},
		// One minute is inside the five-minute refresh lead, so this row refreshes before use.
		{"expiring", time.Now().Add(time.Minute).Unix()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []grokBuildWireCall
			grokbuild.SetTestTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				call := grokBuildWireCall{method: r.Method, rawURL: r.URL.String(), auth: r.Header.Get("Authorization"), userID: r.Header.Get("X-Userid")}
				if r.Method == http.MethodPost {
					raw, _ := io.ReadAll(r.Body)
					form, _ := url.ParseQuery(string(raw))
					call.refresh = form.Get("refresh_token")
					call.client = form.Get("client_id")
					if form.Get("grant_type") != "refresh_token" {
						t.Errorf("grant_type = %s", form.Get("grant_type"))
					}
				}
				mu.Lock()
				calls = append(calls, call)
				mu.Unlock()
				switch {
				case r.Method == http.MethodPost && r.URL.String() == grokbuild.TokenURL:
					return grokBuildTokenHTTP(r, "new-access", "new-refresh", unsignedJWT(`{"sub":"user-filled"}`)), nil
				case r.Method == http.MethodGet && r.URL.String() == grokbuild.BillingURL:
					return grokBuildBillingHTTP(r, http.StatusOK, `{"config":{"creditUsagePercent":23}}`), nil
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					return nil, io.ErrUnexpectedEOF
				}
			}))
			t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

			h := testHandler(t)
			creds := grokBuildDashboardCreds("", "", "old-access", "old-refresh")
			creds.ExpiresAt = tc.expiry
			p := grokBuildDashboardProvider("legacy", creds)
			if err := h.store.CreateProvider(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			card := getUsageCard(t, h, p.ID, false)
			if !strings.Contains(card, "23/100") || strings.Contains(card, "connection changed") || strings.Contains(card, "sign-in expired") {
				t.Fatalf("card = %s", card)
			}
			for _, secret := range []string{"old-access", "old-refresh", "new-access", "new-refresh", "secret@example.com"} {
				if strings.Contains(card, secret) {
					t.Fatalf("card contains %s", secret)
				}
			}
			mu.Lock()
			got := append([]grokBuildWireCall(nil), calls...)
			mu.Unlock()
			if len(got) != 2 || got[0].method != http.MethodPost || got[0].rawURL != grokbuild.TokenURL || got[0].refresh != "old-refresh" || got[0].client != grokbuild.ClientID {
				t.Fatalf("refresh = %+v", got)
			}
			if got[1].method != http.MethodGet || got[1].rawURL != grokbuild.BillingURL || got[1].auth != "Bearer new-access" || got[1].userID != "user-filled" {
				t.Fatalf("billing = %+v", got[1])
			}
			saved, err := h.store.GetProvider(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			conn := saved.OAuthCreds.GrokBuildConnection
			raw, decErr := hex.DecodeString(conn)
			if decErr != nil || len(raw) != 16 || saved.OAuthCreds.AccessToken != "new-access" || saved.OAuthCreds.RefreshToken != "new-refresh" || saved.OAuthCreds.UserID != "user-filled" || saved.OAuthCreds.ExpiresAt <= time.Now().Unix() {
				t.Fatalf("persisted = %+v", saved.OAuthCreds)
			}
			cached := getUsageCard(t, h, p.ID, false)
			if !strings.Contains(cached, "23/100") {
				t.Fatalf("cached card = %s", cached)
			}
			mu.Lock()
			after := len(calls)
			mu.Unlock()
			if after != 2 {
				t.Fatalf("second request was not a cache hit, calls=%d", after)
			}
			saved, err = h.store.GetProvider(context.Background(), p.ID)
			if err != nil || saved.OAuthCreds.GrokBuildConnection != conn || saved.OAuthCreds.AccessToken != "new-access" {
				t.Fatalf("second request rotated again: %+v err=%v", saved.OAuthCreds, err)
			}
		})
	}
}

func TestUsageCardRetriesUnexpiredLegacyGrokBuildAuth(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mu sync.Mutex
			var calls []grokBuildWireCall
			grokbuild.SetTestTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				call := grokBuildWireCall{method: r.Method, rawURL: r.URL.String(), auth: r.Header.Get("Authorization"), userID: r.Header.Get("X-Userid")}
				if r.Method == http.MethodPost {
					raw, _ := io.ReadAll(r.Body)
					form, _ := url.ParseQuery(string(raw))
					call.refresh = form.Get("refresh_token")
					call.client = form.Get("client_id")
				}
				mu.Lock()
				n := len(calls)
				calls = append(calls, call)
				mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.String() == grokbuild.BillingURL && n == 0:
					return grokBuildBillingHTTP(r, status, "upstream-secret-body"), nil
				case r.Method == http.MethodPost && r.URL.String() == grokbuild.TokenURL:
					return grokBuildTokenHTTP(r, "new-access", "new-refresh", ""), nil
				case r.Method == http.MethodGet && r.URL.String() == grokbuild.BillingURL:
					return grokBuildBillingHTTP(r, http.StatusOK, `{"config":{"creditUsagePercent":23}}`), nil
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					return nil, io.ErrUnexpectedEOF
				}
			}))
			t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

			h := testHandler(t)
			creds := grokBuildDashboardCreds("", "user-1", "old-access", "old-refresh")
			p := grokBuildDashboardProvider("legacy", creds)
			if err := h.store.CreateProvider(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			card := getUsageCard(t, h, p.ID, false)
			if !strings.Contains(card, "23/100") || strings.Contains(card, "connection changed") || strings.Contains(card, "not available for this account") {
				t.Fatalf("card = %s", card)
			}
			for _, secret := range []string{"old-access", "old-refresh", "new-access", "new-refresh", "upstream-secret-body", "secret@example.com"} {
				if strings.Contains(card, secret) {
					t.Fatalf("card contains %s", secret)
				}
			}
			mu.Lock()
			got := append([]grokBuildWireCall(nil), calls...)
			mu.Unlock()
			if len(got) != 3 || got[0].method != http.MethodGet || got[0].auth != "Bearer old-access" || got[0].userID != "user-1" {
				t.Fatalf("first read = %+v", got)
			}
			if got[1].method != http.MethodPost || got[1].rawURL != grokbuild.TokenURL || got[1].refresh != "old-refresh" || got[1].client != grokbuild.ClientID {
				t.Fatalf("refresh = %+v", got[1])
			}
			if got[2].method != http.MethodGet || got[2].auth != "Bearer new-access" || got[2].userID != "user-1" {
				t.Fatalf("second read = %+v", got[2])
			}
			saved, err := h.store.GetProvider(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			raw, decErr := hex.DecodeString(saved.OAuthCreds.GrokBuildConnection)
			if decErr != nil || len(raw) != 16 || saved.OAuthCreds.AccessToken != "new-access" || saved.OAuthCreds.RefreshToken != "new-refresh" || saved.OAuthCreds.UserID != "user-1" {
				t.Fatalf("persisted = %+v", saved.OAuthCreds)
			}
			if cached := getUsageCard(t, h, p.ID, false); !strings.Contains(cached, "23/100") {
				t.Fatalf("cached card = %s", cached)
			}
			mu.Lock()
			after := len(calls)
			mu.Unlock()
			if after != 3 {
				t.Fatalf("second request was not a cache hit, calls=%d", after)
			}
		})
	}
}

func TestUsageCardDoesNotAdoptLegacyReconnect(t *testing.T) {
	t.Run("during refresh", func(t *testing.T) {
		runLegacyReconnectRace(t, true)
	})
	t.Run("during auth retry", func(t *testing.T) {
		runLegacyReconnectRace(t, false)
	})
}

func runLegacyReconnectRace(t *testing.T, expired bool) {
	t.Helper()
	started := make(chan struct{})
	var startOnce sync.Once
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	var mu sync.Mutex
	var calls []grokBuildWireCall
	grokbuild.SetTestTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		call := grokBuildWireCall{method: r.Method, rawURL: r.URL.String(), auth: r.Header.Get("Authorization")}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(raw))
			call.refresh = form.Get("refresh_token")
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		if r.Method == http.MethodPost && r.URL.String() == grokbuild.TokenURL {
			startOnce.Do(func() { close(started) })
			<-releaseCh
			return grokBuildTokenHTTP(r, "rotated-access", "rotated-refresh", ""), nil
		}
		if r.Method == http.MethodGet && r.URL.String() == grokbuild.BillingURL && !expired && r.Header.Get("Authorization") == "Bearer old-access" {
			return grokBuildBillingHTTP(r, http.StatusUnauthorized, "upstream-secret-body"), nil
		}
		return grokBuildBillingHTTP(r, http.StatusOK, `{"config":{"creditUsagePercent":91}}`), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	h := testHandler(t)
	creds := grokBuildDashboardCreds("", "user-1", "old-access", "old-refresh")
	if expired {
		creds.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	}
	p := grokBuildDashboardProvider("legacy", creds)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		rr := httptest.NewRecorder()
		sid := itoa(p.ID)
		req := httptest.NewRequest(http.MethodGet, "/dashboard/usage/card/"+sid, nil)
		req.SetPathValue("id", sid)
		h.usageCard(rr, req)
		done <- rr.Body.String()
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("token refresh did not start")
	}
	next := grokBuildDashboardCreds("conn-new", "user-1", "reconnected-access", "reconnected-refresh")
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, next); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	select {
	case body := <-done:
		if strings.Contains(body, "91/100") || strings.Contains(body, "23/100") || !strings.Contains(body, "Grok Build connection changed. Reload and retry.") {
			t.Fatalf("card = %s", body)
		}
		for _, secret := range []string{"old-access", "old-refresh", "rotated-access", "rotated-refresh", "reconnected-access", "reconnected-refresh", "upstream-secret-body"} {
			if strings.Contains(body, secret) {
				t.Fatalf("card contains %s: %s", secret, body)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage card did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	posts, oldBills := 0, 0
	for _, call := range calls {
		switch {
		case call.method == http.MethodPost:
			posts++
			if call.rawURL != grokbuild.TokenURL || call.refresh != "old-refresh" {
				t.Fatalf("refresh call %+v", call)
			}
		case call.method == http.MethodGet && call.auth == "Bearer old-access":
			oldBills++
		default:
			t.Fatalf("adopted call %+v", call)
		}
	}
	if posts != 1 || (expired && oldBills != 0) || (!expired && oldBills != 1) {
		t.Fatalf("posts=%d oldBills=%d calls=%+v", posts, oldBills, calls)
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OAuthCreds.GrokBuildConnection != "conn-new" || saved.OAuthCreds.AccessToken != "reconnected-access" || saved.OAuthCreds.RefreshToken != "reconnected-refresh" {
		t.Fatalf("reconnect was replaced: %+v", saved.OAuthCreds)
	}
}

func unsignedJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + ".x"
}

func grokBuildTokenHTTP(r *http.Request, access, refresh, idToken string) *http.Response {
	body := `{"access_token":"` + access + `","refresh_token":"` + refresh + `","expires_in":3600`
	if idToken != "" {
		body += `,"id_token":"` + idToken + `"`
	}
	body += `}`
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func grokBuildBillingHTTP(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
