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
	"airouter/internal/proxy/responses"
)

func overrideCodexURLs(t *testing.T, usageURL, consumeURL string) {
	t.Helper()
	prevU, prevC := CodexUsageURL, CodexResetConsumeURL
	CodexUsageURL = usageURL
	CodexResetConsumeURL = consumeURL
	t.Cleanup(func() {
		CodexUsageURL = prevU
		CodexResetConsumeURL = prevC
	})
}

func writeCodexUsage(w http.ResponseWriter, credits int) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{"used_percent": 25},
		},
		"rate_limit_reset_credits": map[string]any{"available_count": credits},
	})
}

type consumeCapture struct {
	mu        sync.Mutex
	hits      int
	methods   []string
	headers   []http.Header
	bodies    []map[string]any
	rawBodies [][]byte
}

func (c *consumeCapture) handle(status int, body map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		c.mu.Lock()
		c.hits++
		c.methods = append(c.methods, r.Method)
		c.headers = append(c.headers, r.Header.Clone())
		c.bodies = append(c.bodies, parsed)
		c.rawBodies = append(c.rawBodies, raw)
		c.mu.Unlock()
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

func TestConsumeCodexResetCreditSuccess(t *testing.T) {
	var consume consumeCapture
	var usageHits atomic.Int32
	consumeSrv := httptest.NewServer(consume.handle(http.StatusOK, map[string]any{
		"code": "reset", "windows_reset": 2,
	}))
	t.Cleanup(consumeSrv.Close)
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("usage method = %s", r.Method)
		}
		usageHits.Add(1)
		writeCodexUsage(w, 2)
	}))
	t.Cleanup(usageSrv.Close)
	overrideCodexURLs(t, usageSrv.URL, consumeSrv.URL)

	p := oauthProvider(31, domain.ProtocolOpenAICodex, "tok")
	p.OAuthCreds.AccountID = "acct-1"
	svc := testSvc(t, &stubResolver{token: "tok"})
	result, rep, err := svc.ConsumeCodexResetCredit(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Code != CodexResetReset || result.Message != "Usage reset." {
		t.Fatalf("result = %+v", result)
	}
	if rep == nil || len(rep.Quotas) == 0 || !strings.Contains(rep.Message, "Usage reset.") {
		t.Fatalf("report = %+v", rep)
	}
	if usageHits.Load() != 1 {
		t.Fatalf("usage hits = %d", usageHits.Load())
	}
	if consume.hits != 1 {
		t.Fatalf("consume hits = %d", consume.hits)
	}
	if consume.methods[0] != http.MethodPost {
		t.Fatalf("method = %s", consume.methods[0])
	}
	h := consume.headers[0]
	if h.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", h.Get("Content-Type"))
	}
	if h.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q", h.Get("originator"))
	}
	if h.Get("User-Agent") != "codex_cli_rs/"+responses.CodexCLIVersion {
		t.Fatalf("user-agent = %q", h.Get("User-Agent"))
	}
	if h.Get("chatgpt-account-id") != "acct-1" {
		t.Fatalf("account id = %q", h.Get("chatgpt-account-id"))
	}
	if h.Get("OpenAI-Beta") != "" {
		t.Fatalf("must not send OpenAI-Beta")
	}
	id, _ := consume.bodies[0]["redeem_request_id"].(string)
	if strings.TrimSpace(id) == "" {
		t.Fatalf("body = %+v", consume.bodies[0])
	}
	if _, ok := consume.bodies[0]["credit_id"]; ok {
		t.Fatalf("must not send credit_id: %+v", consume.bodies[0])
	}
}

func TestConsumeCodexResetCreditOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		wantMsg string
		want    CodexResetCode
	}{
		{"already_redeemed", map[string]any{"code": "already_redeemed", "windows_reset": 0}, "Usage reset.", CodexResetAlreadyRedeemed},
		{"nothing_to_reset", map[string]any{"code": "nothing_to_reset", "windows_reset": 0}, "Usage does not need a reset right now.", CodexResetNothingToReset},
		{"no_credit", map[string]any{"code": "no_credit", "windows_reset": 0}, "No reset credits available.", CodexResetNoCredit},
		{"unknown_with_windows", map[string]any{"code": "partial", "windows_reset": 3}, "Usage reset.", CodexResetCode("partial")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var consumeHits, usageHits atomic.Int32
			consumeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				consumeHits.Add(1)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			t.Cleanup(consumeSrv.Close)
			usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				usageHits.Add(1)
				writeCodexUsage(w, 1)
			}))
			t.Cleanup(usageSrv.Close)
			overrideCodexURLs(t, usageSrv.URL, consumeSrv.URL)

			svc := testSvc(t, &stubResolver{token: "tok"})
			result, rep, err := svc.ConsumeCodexResetCredit(context.Background(), oauthProvider(32, domain.ProtocolOpenAICodex, "tok"))
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || result.Code != tc.want || result.Message != tc.wantMsg {
				t.Fatalf("result = %+v", result)
			}
			if rep == nil || rep.Message != tc.wantMsg || len(rep.Quotas) == 0 {
				t.Fatalf("report = %+v", rep)
			}
			if consumeHits.Load() != 1 || usageHits.Load() != 1 {
				t.Fatalf("consume=%d usage=%d", consumeHits.Load(), usageHits.Load())
			}
		})
	}
}

func TestConsumeCodexResetCreditNon200PreservesCache(t *testing.T) {
	var usageHits atomic.Int32
	consumeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(consumeSrv.Close)
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageHits.Add(1)
		writeCodexUsage(w, 3)
	}))
	t.Cleanup(usageSrv.Close)
	overrideCodexURLs(t, usageSrv.URL, consumeSrv.URL)

	now := time.Now()
	svc := testSvc(t, &stubResolver{token: "tok"})
	svc.now = func() time.Time { return now }
	p := oauthProvider(33, domain.ProtocolOpenAICodex, "tok")
	if _, err := svc.Fetch(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if usageHits.Load() != 1 {
		t.Fatalf("seed hits = %d", usageHits.Load())
	}

	result, rep, err := svc.ConsumeCodexResetCredit(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !strings.Contains(result.Message, "unavailable (502)") {
		t.Fatalf("result = %+v", result)
	}
	if rep != nil {
		t.Fatalf("report = %+v", rep)
	}
	if usageHits.Load() != 1 {
		t.Fatalf("consume must not refetch on 502, hits=%d", usageHits.Load())
	}

	cached, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if cached == nil || cached.ResetCredits != 3 {
		t.Fatalf("cached = %+v", cached)
	}
	if usageHits.Load() != 1 {
		t.Fatalf("cache not preserved, hits=%d", usageHits.Load())
	}
}

func TestConsumeCodexResetCreditUnsupportedAndNoToken(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	overrideCodexURLs(t, srv.URL, srv.URL)

	svc := testSvc(t, &stubResolver{token: "tok"})
	_, _, err := svc.ConsumeCodexResetCredit(context.Background(), oauthProvider(34, domain.ProtocolOpenAI, "tok"))
	if err != ErrUnsupported {
		t.Fatalf("unsupported: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("unsupported must not call HTTP, hits=%d", hits.Load())
	}

	svc2 := testSvc(t, &stubResolver{})
	_, _, err = svc2.ConsumeCodexResetCredit(context.Background(), oauthProvider(35, domain.ProtocolOpenAICodex, ""))
	if err != ErrNoToken {
		t.Fatalf("no token: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("no token must not call HTTP, hits=%d", hits.Load())
	}
}

func TestConsumeCodexResetCredit401ReusesRedeemID(t *testing.T) {
	var consume consumeCapture
	var n atomic.Int32
	consumeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		consume.mu.Lock()
		consume.hits++
		consume.methods = append(consume.methods, r.Method)
		consume.headers = append(consume.headers, r.Header.Clone())
		consume.bodies = append(consume.bodies, parsed)
		consume.mu.Unlock()
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "reset", "windows_reset": 1})
	}))
	t.Cleanup(consumeSrv.Close)
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCodexUsage(w, 1)
	}))
	t.Cleanup(usageSrv.Close)
	overrideCodexURLs(t, usageSrv.URL, consumeSrv.URL)

	r := &stubResolver{token: "stale", next: "good"}
	svc := testSvc(t, r)
	result, rep, err := svc.ConsumeCodexResetCredit(context.Background(), oauthProvider(36, domain.ProtocolOpenAICodex, "stale"))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Message != "Usage reset." || rep == nil {
		t.Fatalf("result=%+v report=%+v", result, rep)
	}
	if consume.hits != 2 {
		t.Fatalf("hits = %d", consume.hits)
	}
	id1, _ := consume.bodies[0]["redeem_request_id"].(string)
	id2, _ := consume.bodies[1]["redeem_request_id"].(string)
	if id1 == "" || id1 != id2 {
		t.Fatalf("redeem ids = %q %q", id1, id2)
	}
	if consume.headers[0].Get("Authorization") != "Bearer stale" {
		t.Fatalf("first auth = %q", consume.headers[0].Get("Authorization"))
	}
	if consume.headers[1].Get("Authorization") != "Bearer good" {
		t.Fatalf("second auth = %q", consume.headers[1].Get("Authorization"))
	}
	if r.forceN != 1 {
		t.Fatalf("forceN = %d", r.forceN)
	}
}

func TestDropCacheRejectsStaleInflightWrite(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			close(started)
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "stale",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{"used_percent": 90},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "fresh",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 10},
			},
		})
	}))
	t.Cleanup(up.Close)
	overrideCodexURLs(t, up.URL, "http://127.0.0.1:1")

	svc := testSvc(t, &stubResolver{token: "tok"})
	p := oauthProvider(37, domain.ProtocolOpenAICodex, "tok")

	errc := make(chan error, 1)
	go func() {
		_, err := svc.Fetch(context.Background(), p)
		errc <- err
	}()
	<-started
	svc.dropCache(p.ID)
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}

	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 (stale cache write rejected)", hits.Load())
	}
	if rep.Plan != "fresh" {
		t.Fatalf("plan = %q", rep.Plan)
	}
}
