package usage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

type stubResolver struct {
	mu     sync.Mutex
	token  string
	next   string
	forceN int
	calls  int
	err    error
}

func (s *stubResolver) Resolve(_ context.Context, _ *domain.Provider, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if force {
		s.forceN++
		if s.next != "" {
			s.token = s.next
		}
	}
	return s.token, s.err
}

func testSvc(t *testing.T, r tokenResolver) *Service {
	t.Helper()
	return newService(r, nil, &http.Client{Timeout: 5 * time.Second})
}

func oauthProvider(id int64, proto domain.Protocol, token string) *domain.Provider {
	return &domain.Provider{
		ID:         id,
		Name:       "p",
		Protocol:   proto,
		AuthMethod: domain.AuthOAuth,
		AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: token, ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}
}

func TestFetchCodexHappyPath(t *testing.T) {
	reset := int64(1_800_000_000)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": 25, "reset_at": reset},
				"secondary_window": map[string]any{"percent_used": 80, "resets_at": reset},
			},
			"rate_limit_reset_credits": map[string]any{"available_count": 3},
		})
	}))
	t.Cleanup(up.Close)
	prev := CodexUsageURL
	CodexUsageURL = up.URL + "/backend-api/wham/usage"
	t.Cleanup(func() { CodexUsageURL = prev })

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), oauthProvider(1, domain.ProtocolOpenAICodex, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Plan != "plus" || rep.ResetCredits != 3 {
		t.Fatalf("plan=%q credits=%d", rep.Plan, rep.ResetCredits)
	}
	if len(rep.Quotas) != 2 {
		t.Fatalf("quotas=%d", len(rep.Quotas))
	}
	if rep.Quotas[0].Name != "session (5h)" || rep.Quotas[0].RemainingPct == nil || *rep.Quotas[0].RemainingPct != 75 {
		t.Fatalf("session = %+v", rep.Quotas[0])
	}
	if rep.Quotas[1].Name != "weekly (7d)" || *rep.Quotas[1].RemainingPct != 20 {
		t.Fatalf("weekly = %+v", rep.Quotas[1])
	}
}

func TestFetchCodexSoftFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(up.Close)
	prev := CodexUsageURL
	CodexUsageURL = up.URL
	t.Cleanup(func() { CodexUsageURL = prev })

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), oauthProvider(2, domain.ProtocolOpenAICodex, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Message == "" || !strings.Contains(rep.Message, "502") {
		t.Fatalf("message = %q", rep.Message)
	}
}

func TestFetchClaudeWindows(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Errorf("missing beta")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("missing version")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour":      map[string]any{"utilization": 87, "resets_at": "2026-01-01T00:00:00Z"},
			"seven_day":      map[string]any{"utilization": 10, "resets_at": "2026-01-07T00:00:00Z"},
			"seven_day_opus": map[string]any{"utilization": 40, "resets_at": "2026-01-07T00:00:00Z"},
		})
	}))
	t.Cleanup(up.Close)
	prev := ClaudeUsageURL
	ClaudeUsageURL = up.URL
	t.Cleanup(func() { ClaudeUsageURL = prev })

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), oauthProvider(3, domain.ProtocolClaudeCode, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Plan != "Claude Code" || len(rep.Quotas) != 3 {
		t.Fatalf("plan=%q n=%d", rep.Plan, len(rep.Quotas))
	}
	byName := map[string]Quota{}
	for _, q := range rep.Quotas {
		byName[q.Name] = q
	}
	if *byName["session (5h)"].RemainingPct != 13 {
		t.Fatalf("session remaining = %v", *byName["session (5h)"].RemainingPct)
	}
	if _, ok := byName["weekly opus (7d)"]; !ok {
		t.Fatalf("missing model window: %+v", byName)
	}
}

func TestFetchClaude429Cooldown(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(up.Close)
	prev := ClaudeUsageURL
	ClaudeUsageURL = up.URL
	t.Cleanup(func() { ClaudeUsageURL = prev })

	now := time.Now()
	svc := testSvc(t, &stubResolver{token: "tok"})
	svc.now = func() time.Time { return now }

	p := oauthProvider(4, domain.ProtocolClaudeCode, "tok")
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || !strings.Contains(rep.Message, "429") {
		t.Fatalf("first: err=%v msg=%q", err, rep.Message)
	}
	// Force still respects cooldown.
	rep2, err := svc.FetchWith(context.Background(), p, FetchOpts{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep2.Message, "cooling down") {
		t.Fatalf("cooldown msg = %q", rep2.Message)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	now = now.Add(claudeCooldown + time.Second)
	_, _ = svc.FetchWith(context.Background(), p, FetchOpts{Force: true})
	if hits.Load() != 2 {
		t.Fatalf("after expiry hits = %d", hits.Load())
	}
}

func TestFetch401RetryOnce(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if n < 1 {
			t.Fatal("unreachable")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"userQuota": map[string]any{"total": 100, "used": 10, "remaining": 90, "unit": "credits"},
			"expiresAt": 1_800_000_000_000,
		})
	}))
	t.Cleanup(up.Close)
	prev := QoderUsageURL
	QoderUsageURL = up.URL
	t.Cleanup(func() { QoderUsageURL = prev })

	r := &stubResolver{token: "stale", next: "good"}
	svc := testSvc(t, r)
	rep, err := svc.Fetch(context.Background(), oauthProvider(5, domain.ProtocolQoder, "stale"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Quotas) != 1 || rep.Quotas[0].Used != 10 {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
	if r.forceN != 1 || hits.Load() != 2 {
		t.Fatalf("force=%d hits=%d", r.forceN, hits.Load())
	}
}

func TestFetchQoderSkipsZeroOrg(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"userQuota":          map[string]any{"total": 200, "used": 50, "remaining": 150, "unit": "credits"},
			"orgResourcePackage": map[string]any{"total": 0, "used": 0, "remaining": 0},
			"expiresAt":          1_800_000_000_000,
		})
	}))
	t.Cleanup(up.Close)
	prev := QoderUsageURL
	QoderUsageURL = up.URL
	t.Cleanup(func() { QoderUsageURL = prev })

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), oauthProvider(6, domain.ProtocolQoder, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Quotas) != 1 || rep.Quotas[0].Name != "Personal" {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
	if rep.Quotas[0].ResetAt == nil {
		t.Fatal("missing reset")
	}
}

func TestFetchKiroGETAndPOSTFallback(t *testing.T) {
	var sawGET, sawPOST atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "getUsageLimits") {
			sawGET.Store(true)
			if r.Header.Get("tokentype") != "API_KEY" {
				t.Errorf("missing tokentype on GET")
			}
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("x-amz-target") == "AmazonCodeWhispererService.GetUsageLimits" {
			sawPOST.Store(true)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["profileArn"]; ok {
				t.Errorf("api-key POST must not send placeholder profileArn: %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"subscriptionInfo": map[string]any{"subscriptionTitle": "Kiro Pro"},
				"nextDateReset":    "2026-02-01T00:00:00Z",
				"usageBreakdownList": []any{
					map[string]any{
						"resourceType":              "AGENTIC_REQUEST",
						"currentUsageWithPrecision": 12.5,
						"usageLimitWithPrecision":   100,
						"freeTrialInfo": map[string]any{
							"currentUsageWithPrecision": 1,
							"usageLimitWithPrecision":   20,
							"freeTrialExpiry":           "2026-01-15T00:00:00Z",
						},
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)
	prev, prevQ := KiroUsageBase, KiroQUsageBase
	KiroUsageBase = up.URL
	KiroQUsageBase = up.URL
	t.Cleanup(func() { KiroUsageBase, KiroQUsageBase = prev, prevQ })

	p := &domain.Provider{
		ID:         7,
		Name:       "k",
		Protocol:   domain.ProtocolKiro,
		AuthMethod: domain.AuthAPIKey,
		APIKey:     "k-key",
		OAuthCreds: &domain.OAuthCreds{}, // no ProfileArn
	}
	svc := testSvc(t, nil)
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !sawGET.Load() || !sawPOST.Load() {
		t.Fatalf("sawGET=%v sawPOST=%v", sawGET.Load(), sawPOST.Load())
	}
	if rep.Plan != "Kiro Pro" || len(rep.Quotas) != 2 {
		t.Fatalf("plan=%q quotas=%+v", rep.Plan, rep.Quotas)
	}
	if rep.Quotas[0].Name != "agentic_request" || rep.Quotas[0].Used != 12.5 || rep.Quotas[0].Total != 100 {
		t.Fatalf("row0 = %+v", rep.Quotas[0])
	}
	if !strings.Contains(rep.Quotas[1].Name, "free trial") {
		t.Fatalf("row1 = %+v", rep.Quotas[1])
	}
}

func TestFetchKiroGETSuccess(t *testing.T) {
	var posts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"usageBreakdownList": []any{
				map[string]any{
					"resourceType":              "CREDIT",
					"currentUsageWithPrecision": 2,
					"usageLimitWithPrecision":   10,
				},
			},
		})
	}))
	t.Cleanup(up.Close)
	prev, prevQ := KiroUsageBase, KiroQUsageBase
	KiroUsageBase = up.URL
	KiroQUsageBase = "http://127.0.0.1:1"
	t.Cleanup(func() { KiroUsageBase, KiroQUsageBase = prev, prevQ })

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), oauthProvider(8, domain.ProtocolKiro, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 {
		t.Fatalf("POST should not run after GET success")
	}
	if len(rep.Quotas) != 1 || rep.Quotas[0].Remaining != 8 {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
}

func TestFetchAntigravityMissingProject(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)
	prevM, prevL := AntigravityModelsURL, AntigravityLoadURL
	AntigravityModelsURL, AntigravityLoadURL = up.URL+"/models", up.URL+"/load"
	t.Cleanup(func() { AntigravityModelsURL, AntigravityLoadURL = prevM, prevL })

	p := oauthProvider(9, domain.ProtocolAntigravity, "tok")
	p.OAuthCreds.AntigravityAuth = true
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("must fail closed without ProjectID, hits=%d", hits.Load())
	}
	if !strings.Contains(rep.Message, "project") {
		t.Fatalf("message = %q", rep.Message)
	}
}

func TestFetchAntigravityFiltersInternal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			_ = json.NewEncoder(w).Encode(map[string]any{"currentTier": map[string]any{"name": "Pro"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": map[string]any{
				"gemini-3.7-flash-high": map[string]any{
					"displayName": "Gemini 3.7 Flash (High)",
					"quotaInfo":   map[string]any{"remainingFraction": 0.85, "resetTime": "2026-08-25T12:00:00Z"},
				},
				"internal-model": map[string]any{
					"displayName": "Internal",
					"isInternal":  true,
					"quotaInfo":   map[string]any{"remainingFraction": 0.5},
				},
				"no-quota": map[string]any{"displayName": "None"},
			},
		})
	}))
	t.Cleanup(up.Close)
	prevM, prevL := AntigravityModelsURL, AntigravityLoadURL
	AntigravityModelsURL = up.URL + "/v1internal:fetchAvailableModels"
	AntigravityLoadURL = up.URL + "/v1internal:loadCodeAssist"
	t.Cleanup(func() { AntigravityModelsURL, AntigravityLoadURL = prevM, prevL })

	p := oauthProvider(10, domain.ProtocolAntigravity, "tok")
	p.OAuthCreds.ProjectID = "proj-1"
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Plan != "Pro" || len(rep.Quotas) != 1 {
		t.Fatalf("plan=%q quotas=%+v", rep.Plan, rep.Quotas)
	}
	if rep.Quotas[0].Name != "Gemini 3.7 Flash (High)" || *rep.Quotas[0].RemainingPct != 85 {
		t.Fatalf("row = %+v", rep.Quotas[0])
	}
}

func TestCacheTTLAndSingleFlight(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			close(started)
			<-release
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 10},
			},
		})
	}))
	t.Cleanup(up.Close)
	prev := CodexUsageURL
	CodexUsageURL = up.URL
	t.Cleanup(func() { CodexUsageURL = prev })

	now := time.Now()
	svc := testSvc(t, &stubResolver{token: "tok"})
	svc.now = func() time.Time { return now }
	p := oauthProvider(11, domain.ProtocolOpenAICodex, "tok")

	var wg sync.WaitGroup
	wg.Add(2)
	errc := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, err := svc.Fetch(context.Background(), p)
			errc <- err
		}()
	}
	<-started
	release <- struct{}{}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("single-flight hits = %d", hits.Load())
	}

	_, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("TTL cache hits = %d", hits.Load())
	}

	_, err = svc.FetchWith(context.Background(), p, FetchOpts{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("force hits = %d", hits.Load())
	}

	now = now.Add(cacheTTL + time.Second)
	_, err = svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("expired TTL hits = %d", hits.Load())
	}
}

func TestFetchUnsupportedAndNoToken(t *testing.T) {
	svc := testSvc(t, nil)
	if _, err := svc.Fetch(context.Background(), &domain.Provider{Protocol: domain.ProtocolOpenAI}); err != ErrUnsupported {
		t.Fatalf("unsupported: %v", err)
	}
	if _, err := svc.Fetch(context.Background(), &domain.Provider{Protocol: domain.ProtocolQoder, AuthMethod: domain.AuthAPIKey}); err != ErrNoToken {
		t.Fatalf("no token: %v", err)
	}
}

func TestSoftFailureIsCached(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "x", http.StatusServiceUnavailable)
	}))
	t.Cleanup(up.Close)
	prev := CodexUsageURL
	CodexUsageURL = up.URL
	t.Cleanup(func() { CodexUsageURL = prev })

	now := time.Now()
	svc := testSvc(t, &stubResolver{token: "tok"})
	svc.now = func() time.Time { return now }
	p := oauthProvider(12, domain.ProtocolOpenAICodex, "tok")
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil || rep.Message == "" {
		t.Fatalf("err=%v msg=%q", err, rep.Message)
	}
	_, _ = svc.Fetch(context.Background(), p)
	if hits.Load() != 1 {
		t.Fatalf("soft failure should cache, hits=%d", hits.Load())
	}
}
