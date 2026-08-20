package usage

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

// grokProvider returns a minimal xAI OAuth Grok provider.
func grokProvider(token string) *domain.Provider {
	return &domain.Provider{
		ID:         100,
		Name:       "grok",
		Protocol:   domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth,
		AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Preset:      "xai",
			AccessToken: token,
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		},
	}
}

func TestGrokPlanFromTokenTier(t *testing.T) {
	cases := map[any]string{
		0: "Free",
		1: "SuperGrok",
		2: "X Basic",
		3: "X Premium",
		4: "X Premium Plus",
		5: "SuperGrok Heavy",
		6: "SuperGrok Lite",
		7: "",
	}
	for tier, want := range cases {
		payload, _ := json.Marshal(map[string]any{"tier": tier})
		tok := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
		if got := grokPlanFromToken(tok); got != want {
			t.Fatalf("tier=%v: got %q want %q", tier, got, want)
		}
	}
	if got := grokPlanFromToken("not-a-jwt"); got != "" {
		t.Fatalf("garbage = %q", got)
	}
}

func TestGrokSupportedDetection(t *testing.T) {
	if !isGrok(grokProvider("t")) {
		t.Fatal("xAI OAuth preset must be supported")
	}
	cases := []struct {
		name string
		p    *domain.Provider
	}{
		{"nil", nil},
		{"generic openai apikey", &domain.Provider{Protocol: domain.ProtocolOpenAI, APIKey: "k"}},
		{"xai apikey", &domain.Provider{Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthAPIKey, APIKey: "k", OAuthCreds: &domain.OAuthCreds{Preset: "xai"}}},
		{"unrelated oauth openai", &domain.Provider{Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{Preset: "other", AccessToken: "t"}}},
		{"openai oauth no preset", &domain.Provider{Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{AccessToken: "t"}}},
		{"cursor", &domain.Provider{Protocol: domain.ProtocolCursor, AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{Preset: "xai", CursorAuth: true}}},
		{"codex", &domain.Provider{Protocol: domain.ProtocolOpenAICodex, AuthMethod: domain.AuthOAuth, OAuthCreds: &domain.OAuthCreds{Preset: "xai"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isGrok(tc.p) {
				t.Fatal("must not be grok")
			}
		})
	}

	for _, proto := range []domain.Protocol{
		domain.ProtocolOpenAICodex,
		domain.ProtocolClaudeCode,
		domain.ProtocolKiro,
		domain.ProtocolQoder,
		domain.ProtocolAntigravity,
	} {
		if !Supported(&domain.Provider{Protocol: proto}) {
			t.Fatalf("%s should be supported", proto)
		}
	}
}

func TestGrokFetchBillingAndUserHeaders(t *testing.T) {
	var gotAuth, gotUA, gotTokenAuth, gotIdent, gotVer, gotMode, gotEmail, gotUserID string

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/billing":
			gotAuth = r.Header.Get("Authorization")
			gotUA = r.Header.Get("User-Agent")
			gotTokenAuth = r.Header.Get("x-xai-token-auth")
			gotIdent = r.Header.Get("x-grok-client-identifier")
			gotVer = r.Header.Get("x-grok-client-version")
			gotMode = r.Header.Get("x-grok-client-mode")
			gotEmail = r.Header.Get("x-email")
			gotUserID = r.Header.Get("x-userid")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"config": map[string]any{
					"onDemandCap":      map[string]any{"val": 35},
					"onDemandUsed":     map[string]any{"val": 80},
					"billingPeriodEnd": "2026-07-15T00:00:00Z",
				},
			})
		case "/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"hasGrokCodeAccess": true})
		}
	}))
	t.Cleanup(up.Close)
	prevB, prevU, prevG := GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL
	GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = up.URL+"/v1/billing", up.URL+"/v1/user", up.URL+"/never"
	t.Cleanup(func() { GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = prevB, prevU, prevG })

	p := grokProvider("tok")
	p.OAuthCreds.Email = "user@example.com"

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Message != "" {
		t.Fatalf("message = %q", rep.Message)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotUA != grokUserAgent || gotTokenAuth != grokTokenAuth || gotIdent != grokClientIdent || gotVer != grokClientVer || gotMode != "headless" {
		t.Fatalf("ua=%q tokenAuth=%q ident=%q ver=%q mode=%q", gotUA, gotTokenAuth, gotIdent, gotVer, gotMode)
	}
	if gotEmail != "user@example.com" {
		t.Fatalf("x-email = %q", gotEmail)
	}
	if gotUserID != "" {
		t.Fatalf("x-userid must be absent, got %q", gotUserID)
	}
}

func TestGrokHeadersEmailOmittedWhenEmpty(t *testing.T) {
	h := grokHeaders("")
	if _, ok := h["x-email"]; ok {
		t.Fatalf("empty email must omit x-email: %v", h)
	}
	if grokHeaders("a@b.c")["x-email"] != "a@b.c" {
		t.Fatal("x-email must be set when non-empty")
	}
	if _, ok := h["x-userid"]; ok {
		t.Fatal("x-userid must never be set")
	}
}

func TestGrokParseBillingMonthlyOnDemandPrepaidWeekly(t *testing.T) {
	user := map[string]any{"hasGrokCodeAccess": true}
	billing := map[string]any{
		"config": map[string]any{
			"currentPeriod": map[string]any{
				"type":  "USAGE_PERIOD_TYPE_WEEKLY",
				"start": "2026-07-08T00:00:00+00:00",
				"end":   "2026-07-15T00:00:40+00:00",
			},
			"onDemandCap":      map[string]any{"val": 100},
			"onDemandUsed":     map[string]any{"val": 35},
			"prepaidBalance":   map[string]any{"val": 12.5},
			"billingPeriodEnd": "2026-07-15T00:00:00+00:00",
		},
		"monthlyLimit": map[string]any{"val": 1000},
		"includedUsed": map[string]any{"val": 275},
	}
	parsed := parseGrokBilling(billing, user)
	if parsed.plan != "Grok Code" {
		t.Fatalf("plan = %q", parsed.plan)
	}
	byName := map[string]Quota{}
	for _, q := range parsed.quotas {
		byName[q.Name] = q
	}
	if q := byName["Monthly included"]; q.Used != 275 || q.Total != 1000 || *q.RemainingPct != 72.5 {
		t.Fatalf("monthly = %+v", q)
	}
	if q := byName["On-demand"]; q.Used != 35 || q.Total != 100 || *q.RemainingPct != 65 {
		t.Fatalf("on-demand = %+v", q)
	}
	if q := byName["Prepaid"]; q.Used != 0 || q.Total != 12.5 || *q.RemainingPct != 100 || q.ResetAt != nil {
		t.Fatalf("prepaid = %+v", q)
	}
}

func TestGrokParseWeeklySuperGrokAndPlanTitleCase(t *testing.T) {
	parsed := parseGrokBilling(map[string]any{
		"config": map[string]any{
			"creditUsagePercent": 99.0,
			"productUsage": []any{
				map[string]any{"product": "GrokBuild", "usagePercent": 97.0},
				map[string]any{"product": "GrokImagine", "usagePercent": 2.0},
			},
			"billingPeriodEnd": "2026-07-24T12:42:26.494595+00:00",
		},
	}, map[string]any{"subscriptionTier": "XPremiumPlus", "hasGrokCodeAccess": true})
	if parsed.plan != "XPremiumPlus" {
		t.Fatalf("plan = %q", parsed.plan)
	}
	if len(parsed.quotas) != 1 {
		t.Fatalf("productUsage must not create rows: %+v", parsed.quotas)
	}
	q := parsed.quotas[0]
	if q.Name != "Weekly SuperGrok" || q.Used != 99 || q.Total != 100 || *q.RemainingPct != 1 {
		t.Fatalf("weekly = %+v", q)
	}
}

func TestGrokParseValWrappersAndAliases(t *testing.T) {
	parsed := parseGrokBilling(map[string]any{
		"monthly_limit":  map[string]any{"val": "200"},
		"included_used":  map[string]any{"val": "50"},
		"total_used":     map[string]any{"val": "60"},
		"onDemandCap":    "300",
		"onDemandUsed":   "120",
		"prepaidBalance": "30",
		"resetAt":        "2026-08-01T00:00:00Z",
	}, map[string]any{"subscription_tier": "premium_plus"})
	if parsed.plan != "Premium Plus" {
		t.Fatalf("plan = %q", parsed.plan)
	}
	byName := map[string]Quota{}
	for _, q := range parsed.quotas {
		byName[q.Name] = q
	}
	if q := byName["Monthly included"]; q.Used != 50 || q.Total != 200 {
		t.Fatalf("monthly = %+v", q)
	}
	if q := byName["On-demand"]; q.Used != 120 || q.Total != 300 {
		t.Fatalf("on-demand = %+v", q)
	}
	if q := byName["Prepaid"]; q.Used != 0 || q.Total != 30 {
		t.Fatalf("prepaid = %+v", q)
	}
}

func TestGrokParseWeeklySuperGrokValWrapper(t *testing.T) {
	parsed := parseGrokBilling(map[string]any{
		"credit_usage_percent": map[string]any{"val": 40},
		"billingPeriodEnd":     "2026-07-24T00:00:00Z",
	}, map[string]any{"subscriptionTier": "super_grok"})
	if len(parsed.quotas) != 1 {
		t.Fatalf("quotas = %+v", parsed.quotas)
	}
	q := parsed.quotas[0]
	if q.Used != 40 || q.Total != 100 || *q.RemainingPct != 60 {
		t.Fatalf("weekly = %+v", q)
	}
}

func TestGrokParseGenericCreditValWrappers(t *testing.T) {
	parsed := parseGrokBilling(map[string]any{
		"credits": map[string]any{
			"total":     map[string]any{"val": 200},
			"remaining": map[string]any{"val": 75},
			"resetAt":   "2026-08-01T00:00:00Z",
		},
	}, nil)
	if len(parsed.quotas) != 1 {
		t.Fatalf("quotas = %+v", parsed.quotas)
	}
	q := parsed.quotas[0]
	if q.Name != "Credits" || q.Used != 125 || q.Total != 200 || q.Remaining != 75 || *q.RemainingPct != 37.5 || q.ResetAt == nil {
		t.Fatalf("credits = %+v", q)
	}
}

func TestGrokQuotaClampsNegativeUsage(t *testing.T) {
	q := grokQuota("Credits", -5, 100, nil, false)
	if q.Used != 0 || q.Remaining != 100 || q.RemainingPct == nil || *q.RemainingPct != 100 {
		t.Fatalf("quota = %+v", q)
	}
}

func TestGrokParseFreeZeroCapSyntheticExhausted(t *testing.T) {
	parsed := parseGrokBilling(map[string]any{
		"config": map[string]any{
			"onDemandCap":          map[string]any{"val": 0},
			"onDemandUsed":         map[string]any{"val": 0},
			"isUnifiedBillingUser": true,
			"billingPeriodEnd":     "2026-07-15T00:00:00Z",
		},
	}, map[string]any{"hasGrokCodeAccess": true})
	if len(parsed.quotas) != 1 {
		t.Fatalf("quotas = %+v", parsed.quotas)
	}
	q := parsed.quotas[0]
	if q.Name != "On-demand" || q.Used != 1 || q.Total != 1 || *q.RemainingPct != 0 {
		t.Fatalf("on-demand = %+v", q)
	}
}

func TestGrokParsePaidZeroCapNotExhausted(t *testing.T) {
	parsed := parseGrokBilling(map[string]any{
		"config": map[string]any{"onDemandCap": map[string]any{"val": 0}, "onDemandUsed": map[string]any{"val": 0}},
	}, map[string]any{"subscriptionTier": "XPremiumPlus"})
	if parsed.plan != "XPremiumPlus" {
		t.Fatalf("plan = %q", parsed.plan)
	}
	if !parsed.subscriptionAccess {
		t.Fatal("subscriptionAccess should be true")
	}
	if len(parsed.quotas) != 0 {
		t.Fatalf("paid zero-cap must not render exhausted row: %+v", parsed.quotas)
	}
}

func TestGrokFetchUserFailureRetainsBilling(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/user" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{
				"onDemandCap":      map[string]any{"val": 100},
				"onDemandUsed":     map[string]any{"val": 20},
				"billingPeriodEnd": "2026-07-15T00:00:00Z",
			},
		})
	}))
	t.Cleanup(up.Close)
	prevB, prevU, prevG := GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL
	GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = up.URL+"/v1/billing", up.URL+"/v1/user", up.URL+"/never"
	t.Cleanup(func() { GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = prevB, prevU, prevG })

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), grokProvider("tok"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Message != "" {
		t.Fatalf("message = %q", rep.Message)
	}
	if len(rep.Quotas) != 1 || rep.Quotas[0].Name != "On-demand" {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
}

func TestGrokFetch401ForceRefreshOnce(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{
				"onDemandCap":      map[string]any{"val": 100},
				"onDemandUsed":     map[string]any{"val": 10},
				"billingPeriodEnd": "2026-07-15T00:00:00Z",
			},
		})
	}))
	t.Cleanup(up.Close)
	prevB, prevU, prevG := GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL
	GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = up.URL+"/v1/billing", up.URL+"/v1/user", up.URL+"/never"
	t.Cleanup(func() { GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = prevB, prevU, prevG })

	r := &stubResolver{token: "stale", next: "good"}
	svc := testSvc(t, r)
	rep, err := svc.Fetch(context.Background(), grokProvider("stale"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Message != "" || len(rep.Quotas) != 1 {
		t.Fatalf("msg=%q quotas=%+v", rep.Message, rep.Quotas)
	}
	if r.forceN != 1 {
		t.Fatalf("force refreshes = %d, want 1", r.forceN)
	}
	// billing + user initial, then billing + user after refresh.
	if hits.Load() != 4 {
		t.Fatalf("hits = %d, want 4 (billing+user x2)", hits.Load())
	}
}

func TestGrokFetch401NoLoop(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(up.Close)
	prevB, prevU, prevG := GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL
	GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = up.URL, up.URL, up.URL+"/never"
	t.Cleanup(func() { GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = prevB, prevU, prevG })

	r := &stubResolver{token: "stale", next: "stale"}
	svc := testSvc(t, r)
	rep, err := svc.Fetch(context.Background(), grokProvider("stale"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Message, "expired") {
		t.Fatalf("message = %q", rep.Message)
	}
	if r.forceN != 1 {
		t.Fatalf("force refreshes = %d, want exactly 1", r.forceN)
	}
	// billing + user initial, then billing + user retry with refreshed (equal) token.
	if hits.Load() != 4 {
		t.Fatalf("hits = %d, want 4 (no loop)", hits.Load())
	}
}

// gRPC helper JS encoders.

func encodeGrokVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	out = append(out, byte(v))
	return out
}

func encodeGrokTag(field int, wire int) []byte {
	return encodeGrokVarint(uint64(field<<3 | wire))
}

func encodeGrokFixed32(field int, value float32) []byte {
	tag := encodeGrokTag(field, gwireFixed32)
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, math.Float32bits(value))
	return append(tag, body...)
}

func encodeGrokFixed64(field int, value float64) []byte {
	tag := encodeGrokTag(field, gwireFixed64)
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, math.Float64bits(value))
	return append(tag, body...)
}

func encodeGrokVarintField(field int, value uint64) []byte {
	return append(encodeGrokTag(field, gwireVarint), encodeGrokVarint(value)...)
}

func encodeGrokLengthDelimited(field int, body []byte) []byte {
	return append(append(encodeGrokTag(field, gwireLengthDelimited), encodeGrokVarint(uint64(len(body)))...), body...)
}

// buildGrokCreditsFrame returns a gRPC-web framed GetGrokCreditsConfig response.
func buildGrokCreditsFrame(t *testing.T, usageRatio float32, resetSeconds int64, resetNanos int32) []byte {
	t.Helper()
	creditsInfo := encodeGrokFixed32(gtagUsageRatio, usageRatio)
	if resetSeconds != 0 || resetNanos != 0 {
		var ts []byte
		if resetSeconds != 0 {
			ts = append(ts, encodeGrokVarintField(gtagTimestampSec, uint64(resetSeconds))...)
		}
		if resetNanos != 0 {
			ts = append(ts, encodeGrokVarintField(gtagTimestampNanos, uint64(resetNanos))...)
		}
		creditsInfo = append(creditsInfo, encodeGrokLengthDelimited(gtagResetTimestamp, ts)...)
	}
	top := encodeGrokLengthDelimited(gtagTopCredits, creditsInfo)
	frame := make([]byte, 5+len(top))
	frame[0] = 0x00
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(top)))
	copy(frame[5:], top)
	return frame
}

func TestGrokDecoderFixed32WithTimestamp(t *testing.T) {
	b := buildGrokCreditsFrame(t, 0.35, 1784825940, 867850000)
	d, ok := decodeGrokCreditsFrame(b)
	if !ok {
		t.Fatal("decode failed")
	}
	want := time.UnixMilli(1784825940*1000 + 868).UTC()
	if d.resetAt == nil || !d.resetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", d.resetAt, want)
	}
	// float32(0.35) is 0.349999988… so percentUsed is 34.9999…; the quota
	// builder rounds to 35 for display.
	q := grokGrpcQuota(d)
	if q.Used != 35 || q.Total != 100 || *q.RemainingPct != 65 {
		t.Fatalf("quota = %+v", q)
	}
}

func TestGrokDecoderUnframedFixed64(t *testing.T) {
	creditsInfo := encodeGrokFixed64(gtagUsageRatio, 0.4)
	top := encodeGrokLengthDelimited(gtagTopCredits, creditsInfo)
	d, ok := decodeGrokCreditsFrame(top)
	if !ok {
		t.Fatal("decode failed")
	}
	if d.percentUsed != 40 {
		t.Fatalf("percentUsed = %v", d.percentUsed)
	}
}

func TestGrokDecoderRejectsNonFiniteRatios(t *testing.T) {
	for _, ratio := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		creditsInfo := encodeGrokFixed64(gtagUsageRatio, ratio)
		top := encodeGrokLengthDelimited(gtagTopCredits, creditsInfo)
		if _, ok := decodeGrokCreditsFrame(top); ok {
			t.Fatalf("ratio %v should fail", ratio)
		}
	}
}

func TestGrokDecoderTrailerHandling(t *testing.T) {
	trailer := make([]byte, 5)
	trailer[0] = 0x80
	binary.BigEndian.PutUint32(trailer[1:5], 0)
	if got := findGrokDataPayload(trailer); got != nil {
		t.Fatalf("trailer-only frame should return nil, got %v", got)
	}

	framed := append(buildGrokCreditsFrame(t, 0.25, 0, 0), trailer...)
	d, ok := decodeGrokCreditsFrame(framed)
	if !ok || math.Abs(d.percentUsed-25) > 0.001 {
		t.Fatalf("data plus trailer = %+v, ok=%v", d, ok)
	}
}

func TestGrokDecoderMalformedFailOpen(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{},
		{0x05, 0x00, 0x00, 0x00, 0x00},
		{0x08, 0x03, 0xff},
		{0xff, 0xff, 0xff, 0xff, 0xff},
		encodeGrokVarint(1 << 10),
	} {
		if _, ok := decodeGrokCreditsFrame(b); ok {
			t.Fatalf("should fail open on %v", b)
		}
	}
}

func TestGrokGrpcFetchEndpointHeadersBody(t *testing.T) {
	var gotMethod, gotAuth, gotCT, gotGrpcWeb, gotAccept string
	var gotBody []byte

	grpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotGrpcWeb = r.Header.Get("X-Grpc-Web")
		gotAccept = r.Header.Get("Accept")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		_, _ = w.Write(buildGrokCreditsFrame(t, 0.35, 1784825940, 867850000))
	}))
	t.Cleanup(grpcServer.Close)

	restServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/user") {
			_ = json.NewEncoder(w).Encode(map[string]any{"subscriptionTier": "XPremiumPlus"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{
				"onDemandCap":      map[string]any{"val": 0},
				"onDemandUsed":     map[string]any{"val": 0},
				"billingPeriodEnd": "2026-07-15T00:00:00Z",
			},
		})
	}))
	t.Cleanup(restServer.Close)

	prevB, prevU, prevG := GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL
	GrokBillingURL = restServer.URL + "/v1/billing"
	GrokUserURL = restServer.URL + "/v1/user"
	GrokGrpcCreditsURL = grpcServer.URL + "/grpc"
	t.Cleanup(func() { GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = prevB, prevU, prevG })

	svc := testSvc(t, &stubResolver{token: "tok"})
	p := grokProvider("tok")

	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Quotas) != 1 || rep.Quotas[0].Name != "Weekly SuperGrok" {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotAuth != "Bearer tok" || gotCT != "application/grpc-web+proto" || gotGrpcWeb != "1" || gotAccept != "application/grpc-web+proto" {
		t.Fatalf("headers auth=%q ct=%q grpc-web=%q accept=%q", gotAuth, gotCT, gotGrpcWeb, gotAccept)
	}
	if string(gotBody) != "\x00\x00\x00\x00\x00" {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestGrokGrpcSoftMessageWhenFallbackFails(t *testing.T) {
	grpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(grpcServer.Close)

	restServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/user") {
			_ = json.NewEncoder(w).Encode(map[string]any{"subscriptionTier": "XPremiumPlus"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{
				"onDemandCap":      map[string]any{"val": 0},
				"onDemandUsed":     map[string]any{"val": 0},
				"billingPeriodEnd": "2026-07-15T00:00:00Z",
			},
		})
	}))
	t.Cleanup(restServer.Close)

	prevB, prevU, prevG := GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL
	GrokBillingURL = restServer.URL + "/v1/billing"
	GrokUserURL = restServer.URL + "/v1/user"
	GrokGrpcCreditsURL = grpcServer.URL + "/grpc"
	t.Cleanup(func() { GrokBillingURL, GrokUserURL, GrokGrpcCreditsURL = prevB, prevU, prevG })

	svc := testSvc(t, &stubResolver{token: "tok"})
	p := grokProvider("tok")
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Message == "" || !strings.Contains(rep.Message, "active") {
		t.Fatalf("message = %q", rep.Message)
	}
}

func TestGrokGrpcQuota(t *testing.T) {
	d := grokCreditsDecoded{percentUsed: 80, resetAt: nil}
	q := grokGrpcQuota(d)
	if q.Name != "Weekly SuperGrok" || q.Used != 80 || q.Total != 100 || *q.RemainingPct != 20 || q.ResetAt != nil {
		t.Fatalf("quota = %+v", q)
	}
}
