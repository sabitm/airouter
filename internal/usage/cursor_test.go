package usage

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/cursor"
)

func cursorProvider(token, machineID string) *domain.Provider {
	return &domain.Provider{
		ID:         200,
		Name:       "cursor",
		Protocol:   domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth,
		AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth:  true,
			AccessToken: token,
			MachineID:   machineID,
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		},
	}
}

func TestCursorSupported(t *testing.T) {
	if !Supported(&domain.Provider{Protocol: domain.ProtocolCursor}) {
		t.Fatal("ProtocolCursor must be supported")
	}
	oauthP := cursorProvider("t", "m1")
	if !Supported(oauthP) {
		t.Fatal("Cursor OAuth must be supported")
	}
	imported := &domain.Provider{
		Protocol:   domain.ProtocolCursor,
		AuthMethod: domain.AuthAPIKey,
		APIKey:     "imported-token",
	}
	if !Supported(imported) {
		t.Fatal("imported Cursor token providers must be supported")
	}
	if isGrok(oauthP) {
		t.Fatal("Cursor must not match the Grok gate")
	}
}

func encodeCursorVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func encodeCursorTag(field, wire int) []byte {
	return encodeCursorVarint(uint64(field<<3 | wire))
}

func encodeCursorVarintField(field int, value uint64) []byte {
	return append(encodeCursorTag(field, cursorWireVarint), encodeCursorVarint(value)...)
}

func encodeCursorInt32Field(field int, value int32) []byte {
	return encodeCursorVarintField(field, uint64(value))
}

func encodeCursorLenField(field int, body []byte) []byte {
	return append(append(encodeCursorTag(field, cursorWireLen), encodeCursorVarint(uint64(len(body)))...), body...)
}

func encodeCursorStringField(field int, s string) []byte {
	return encodeCursorLenField(field, []byte(s))
}

func encodeCursorDoubleField(field int, value float64) []byte {
	tag := encodeCursorTag(field, cursorWireFixed64)
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, math.Float64bits(value))
	return append(tag, body...)
}

func buildPeriodUsage(planUsage, spendLimit []byte, cycleEndMs int64) []byte {
	var out []byte
	if cycleEndMs != 0 {
		out = append(out, encodeCursorVarintField(2, uint64(cycleEndMs))...)
	}
	if planUsage != nil {
		out = append(out, encodeCursorLenField(3, planUsage)...)
	}
	if spendLimit != nil {
		out = append(out, encodeCursorLenField(4, spendLimit)...)
	}
	return out
}

func TestCursorDecodeStandardPlanUsagePercentsAndReset(t *testing.T) {
	resetMs := int64(1_800_000_000_000)
	planUsage := append(append(
		encodeCursorInt32Field(2, 2500),
		encodeCursorInt32Field(5, 10000)...),
		encodeCursorDoubleField(14, 42.5)...,
	)
	spend := append(append(
		encodeCursorInt32Field(6, 1500),
		encodeCursorInt32Field(5, 20000)...),
		encodeCursorStringField(8, "user")...,
	)
	period, ok := decodeCursorPeriodUsage(buildPeriodUsage(planUsage, spend, resetMs))
	if !ok || period.planUsage == nil || period.spendLimit == nil {
		t.Fatalf("decode failed: ok=%v %+v", ok, period)
	}
	if period.billingCycleEnd != resetMs {
		t.Fatalf("billingCycleEnd = %d", period.billingCycleEnd)
	}
	if period.planUsage.includedSpend != 2500 || period.planUsage.limit != 10000 {
		t.Fatalf("planUsage = %+v", period.planUsage)
	}
	if period.planUsage.totalPercentUsed == nil || *period.planUsage.totalPercentUsed != 42.5 {
		t.Fatalf("percent = %v", period.planUsage.totalPercentUsed)
	}
	if period.spendLimit.individualUsed != 1500 || period.spendLimit.individualLimit == nil || *period.spendLimit.individualLimit != 20000 {
		t.Fatalf("spend = %+v", period.spendLimit)
	}

	hard := &cursorHardLimit{hardLimit: 20}
	planInfo := &cursorPlanInfo{planName: "Pro"}
	rep := buildCursorStandardReport(period, hard, planInfo)
	if rep.Plan != "Pro" {
		t.Fatalf("plan = %q", rep.Plan)
	}
	if len(rep.Quotas) != 2 {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
	inc := rep.Quotas[0]
	if inc.Name != "Included" || inc.RemainingPct == nil || *inc.RemainingPct != 57.5 || inc.Total != 0 || inc.Used != 0 {
		t.Fatalf("included = %+v", inc)
	}
	if inc.ResetAt == nil || !inc.ResetAt.Equal(time.UnixMilli(resetMs).UTC()) {
		t.Fatalf("reset = %v", inc.ResetAt)
	}
	od := rep.Quotas[1]
	if od.Name != "On-demand" || od.Unit != "$" || od.Used != 15 || od.Total != 20 || od.Remaining != 5 {
		t.Fatalf("on-demand = %+v", od)
	}
}

func TestCursorDecodeAbsentPercentFallsBackToSpendOverLimit(t *testing.T) {
	planUsage := append(encodeCursorInt32Field(2, 25), encodeCursorInt32Field(5, 100)...)
	period, ok := decodeCursorPeriodUsage(buildPeriodUsage(planUsage, nil, 0))
	if !ok || period.planUsage == nil || period.planUsage.totalPercentUsed != nil {
		t.Fatalf("decode = ok=%v %+v", ok, period.planUsage)
	}
	if got := includedPercentUsed(period.planUsage); got != 25 {
		t.Fatalf("percent = %v", got)
	}
}

func TestCursorDecodeMalformedNonfiniteNegative(t *testing.T) {
	if _, ok := decodeCursorPeriodUsage([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}); ok {
		t.Fatal("garbage should fail")
	}
	if _, ok := decodeCursorPeriodUsage(nil); !ok {
		t.Fatal("empty protobuf message is valid")
	}

	nanBody := encodeCursorLenField(3, encodeCursorDoubleField(14, math.NaN()))
	if _, ok := decodeCursorPeriodUsage(nanBody); ok {
		t.Fatal("NaN percent should fail open")
	}
	infBody := encodeCursorLenField(3, encodeCursorDoubleField(14, math.Inf(1)))
	if _, ok := decodeCursorPeriodUsage(infBody); ok {
		t.Fatal("Inf percent should fail open")
	}

	negPlan := encodeCursorLenField(3, append(encodeCursorInt32Field(2, -5), encodeCursorInt32Field(5, 100)...))
	period, ok := decodeCursorPeriodUsage(negPlan)
	if !ok || period.planUsage == nil {
		t.Fatal("negative included_spend should clamp, not fail")
	}
	if period.planUsage.includedSpend != 0 {
		t.Fatalf("includedSpend = %d, want clamped 0", period.planUsage.includedSpend)
	}
	if got := includedPercentUsed(period.planUsage); got != 0 {
		t.Fatalf("percent from negative spend = %v", got)
	}

	over := encodeCursorLenField(3, encodeCursorDoubleField(14, 250))
	period, ok = decodeCursorPeriodUsage(over)
	if !ok || includedPercentUsed(period.planUsage) != 100 {
		t.Fatalf("over-100 percent should clamp: %+v", period.planUsage)
	}
}

func TestCursorOnDemandTeamAndPersonalKinds(t *testing.T) {
	limit := int32(5000)
	spend := &cursorSpendLimit{individualUsed: 100, individualLimit: &limit, limitType: "team"}
	q, ok, _ := cursorOnDemandQuota(spend, nil, nil)
	if !ok || q.Total != 50 || q.Used != 1 {
		t.Fatalf("team individualLimit fixed = %+v ok=%v", q, ok)
	}

	zero := int32(0)
	spend = &cursorSpendLimit{individualUsed: 0, individualLimit: &zero, limitType: "team"}
	if _, ok, note := cursorOnDemandQuota(spend, nil, nil); ok || note != "On-demand usage is disabled." {
		t.Fatalf("team zero individualLimit: ok=%v note=%q", ok, note)
	}

	spend = &cursorSpendLimit{individualUsed: 200, limitType: "team"}
	if _, ok, note := cursorOnDemandQuota(spend, &cursorHardLimit{noUsageBasedAllowed: true, hardLimit: 10}, nil); ok || note != "On-demand usage is disabled." {
		t.Fatalf("team hardLimit disabled: ok=%v note=%q", ok, note)
	}
	if _, ok, note := cursorOnDemandQuota(spend, &cursorHardLimit{hardLimit: 25}, nil); ok || note != "" {
		t.Fatalf("team unlimited member should omit row: ok=%v note=%q", ok, note)
	}

	pooled := int32(1000)
	spend = &cursorSpendLimit{individualUsed: 0, pooledLimit: &pooled, limitType: "team"}
	if _, ok, note := cursorOnDemandQuota(spend, nil, nil); ok || note != "" {
		t.Fatalf("team pooled unlimited: ok=%v note=%q", ok, note)
	}
	spend = &cursorSpendLimit{limitType: "team"}
	if _, ok, note := cursorOnDemandQuota(spend, nil, nil); ok || note != "On-demand usage is unavailable." {
		t.Fatalf("team unavailable: ok=%v note=%q", ok, note)
	}

	if _, ok, note := cursorOnDemandQuota(&cursorSpendLimit{individualUsed: 10}, nil, nil); ok || note != "On-demand usage is unavailable." {
		t.Fatalf("personal no hardLimit: ok=%v note=%q", ok, note)
	}
	if _, ok, note := cursorOnDemandQuota(nil, &cursorHardLimit{noUsageBasedAllowed: true}, nil); ok || note != "On-demand usage is disabled." {
		t.Fatalf("personal noUsageBasedAllowed: ok=%v note=%q", ok, note)
	}
	if _, ok, note := cursorOnDemandQuota(nil, &cursorHardLimit{hardLimit: cursorHardLimitUnlimited}, nil); ok || note != "" {
		t.Fatalf("personal unlimited: ok=%v note=%q", ok, note)
	}
	q, ok, _ = cursorOnDemandQuota(&cursorSpendLimit{individualUsed: 2500}, &cursorHardLimit{hardLimit: 40}, nil)
	if !ok || q.Used != 25 || q.Total != 40 || q.Unit != "$" {
		t.Fatalf("personal fixed dollars = %+v ok=%v", q, ok)
	}
}

func cursorTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	prev := CursorDashboardBaseURL
	CursorDashboardBaseURL = up.URL
	t.Cleanup(func() { CursorDashboardBaseURL = prev })
	return up
}

func TestCursorFetchHeadersPathsEmptyBody(t *testing.T) {
	var periodHits, hardHits, planHits atomic.Int32
	var gotMethod, gotAccept, gotCT, gotConnectVersion, gotConnectTimeout, gotAuth, gotClientType, gotClientVer string
	var gotBody []byte
	var gotPath string
	var sawAgentOrIDE atomic.Bool

	planUsage := append(encodeCursorInt32Field(2, 10), encodeCursorInt32Field(5, 100)...)
	spend := append(encodeCursorInt32Field(6, 300), encodeCursorInt32Field(5, 10000)...)
	periodBody := buildPeriodUsage(planUsage, spend, 1_800_000_000_000)
	hardBody := encodeCursorInt32Field(1, 50)
	planBody := encodeCursorLenField(1, encodeCursorStringField(1, "Pro"))

	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
			periodHits.Add(1)
			gotMethod = r.Method
			gotAccept = r.Header.Get("Accept")
			gotCT = r.Header.Get("Content-Type")
			gotConnectVersion = r.Header.Get("Connect-Protocol-Version")
			gotConnectTimeout = r.Header.Get("Connect-Timeout-Ms")
			gotAuth = r.Header.Get("Authorization")
			gotClientType = r.Header.Get("X-Cursor-Client-Type")
			gotClientVer = r.Header.Get("X-Cursor-Client-Version")
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			for _, name := range []string{
				"X-Client-Key",
				"X-Cursor-Checksum",
				"X-Session-Id",
				"X-Cursor-Client-Commit",
				"X-Cursor-Client-OS",
				"X-Cursor-Client-Arch",
				"X-Cursor-Client-Device-Type",
				"X-Cursor-Config-Version",
				"X-Cursor-Timezone",
			} {
				if r.Header.Get(name) != "" {
					sawAgentOrIDE.Store(true)
				}
			}
			w.Header().Set("Content-Type", cursor.ProtoContentType)
			_, _ = w.Write(periodBody)
		case "/aiserver.v1.DashboardService/GetHardLimit":
			hardHits.Add(1)
			w.Header().Set("Content-Type", cursor.ProtoContentType)
			_, _ = w.Write(hardBody)
		case "/aiserver.v1.DashboardService/GetPlanInfo":
			planHits.Add(1)
			w.Header().Set("Content-Type", cursor.ProtoContentType)
			_, _ = w.Write(planBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	p := cursorProvider("tok", "machine-1")
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Plan != "Pro" || len(rep.Quotas) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if periodHits.Load() != 1 || hardHits.Load() != 1 || planHits.Load() != 1 {
		t.Fatalf("hits period=%d hard=%d plan=%d", periodHits.Load(), hardHits.Load(), planHits.Load())
	}
	if gotPath != "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q", gotMethod)
	}
	if len(gotBody) != 0 {
		t.Fatalf("body = %v", gotBody)
	}
	if gotAccept != cursor.ProtoContentType || gotCT != cursor.ProtoContentType {
		t.Fatalf("accept=%q ct=%q", gotAccept, gotCT)
	}
	if gotConnectVersion != "1" {
		t.Fatalf("connect protocol version = %q", gotConnectVersion)
	}
	if gotConnectTimeout != "10000" {
		t.Fatalf("connect timeout = %q", gotConnectTimeout)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotClientType != cursor.ClientType || gotClientVer != cursor.ClientVersion {
		t.Fatalf("client type=%q ver=%q", gotClientType, gotClientVer)
	}
	if sawAgentOrIDE.Load() {
		t.Fatal("AgentService and IDE-only identity headers must be absent")
	}
}

func TestCursorOptionalRPCFailureRetainsPeriodUsage(t *testing.T) {
	planUsage := append(encodeCursorInt32Field(2, 40), encodeCursorInt32Field(5, 100)...)
	periodBody := buildPeriodUsage(planUsage, nil, 0)

	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
			_, _ = w.Write(periodBody)
		case "/aiserver.v1.DashboardService/GetHardLimit", "/aiserver.v1.DashboardService/GetPlanInfo":
			http.Error(w, "nope", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})

	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Quotas) != 1 || rep.Quotas[0].Name != "Included" {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
	if rep.Plan != "Current plan" {
		t.Fatalf("plan fallback = %q", rep.Plan)
	}
}

func TestCursorFetch401ForceRefreshOnce(t *testing.T) {
	var periodHits atomic.Int32
	planUsage := append(encodeCursorInt32Field(2, 10), encodeCursorInt32Field(5, 100)...)
	periodBody := buildPeriodUsage(planUsage, nil, 0)

	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
			periodHits.Add(1)
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write(periodBody)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(nil)
	})

	r := &stubResolver{token: "stale", next: "good"}
	svc := testSvc(t, r)
	rep, err := svc.Fetch(context.Background(), cursorProvider("stale", "m1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Quotas) != 1 || rep.Quotas[0].Name != "Included" {
		t.Fatalf("quotas = %+v msg=%q", rep.Quotas, rep.Message)
	}
	if r.forceN != 1 {
		t.Fatalf("force refreshes = %d, want 1", r.forceN)
	}
	if periodHits.Load() != 2 {
		t.Fatalf("period hits = %d, want 2", periodHits.Load())
	}
}

func TestCursorFetch401NoLoop(t *testing.T) {
	var periodHits atomic.Int32
	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
			periodHits.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
	})

	r := &stubResolver{token: "stale", next: "stale"}
	svc := testSvc(t, r)
	rep, err := svc.Fetch(context.Background(), cursorProvider("stale", "m1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Message, "expired") {
		t.Fatalf("message = %q", rep.Message)
	}
	if r.forceN != 1 {
		t.Fatalf("force refreshes = %d, want 1", r.forceN)
	}
	if periodHits.Load() != 2 {
		t.Fatalf("period hits = %d, want 2 (no loop)", periodHits.Load())
	}
}

func TestCursorFetch401StaticTokenDoesNotRefresh(t *testing.T) {
	var periodHits atomic.Int32
	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
			periodHits.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
	})

	r := &stubResolver{token: "imported"}
	svc := testSvc(t, r)
	p := &domain.Provider{
		ID:         201,
		Protocol:   domain.ProtocolCursor,
		AuthMethod: domain.AuthAPIKey,
		APIKey:     "imported",
	}
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Message, "expired") {
		t.Fatalf("message = %q", rep.Message)
	}
	if r.forceN != 0 {
		t.Fatalf("force refreshes = %d, want 0", r.forceN)
	}
	if periodHits.Load() != 1 {
		t.Fatalf("period hits = %d, want 1", periodHits.Load())
	}
}

func TestCursorFetch401InvalidGrantSurfaces(t *testing.T) {
	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	r := &countingGrantResolver{first: "stale"}
	svc := testSvc(t, r)
	_, err := svc.Fetch(context.Background(), cursorProvider("stale", "m1"))
	if err == nil || !strings.Contains(err.Error(), "invalid or revoked") {
		t.Fatalf("err = %v", err)
	}
	if r.forceN != 1 {
		t.Fatalf("force = %d", r.forceN)
	}
}

type countingGrantResolver struct {
	first  string
	forceN int
	calls  int
}

func (s *countingGrantResolver) Resolve(_ context.Context, _ *domain.Provider, force bool) (string, error) {
	s.calls++
	if force {
		s.forceN++
		return "", oauth.ErrInvalidGrant
	}
	return s.first, nil
}

func TestCursorSoftFailures(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		prev := CursorDashboardBaseURL
		CursorDashboardBaseURL = "http://127.0.0.1:1"
		t.Cleanup(func() { CursorDashboardBaseURL = prev })
		svc := testSvc(t, &stubResolver{token: "tok"})
		rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rep.Message, "Unable to fetch usage") {
			t.Fatalf("message = %q", rep.Message)
		}
	})
	t.Run("status", func(t *testing.T) {
		cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusBadGateway)
		})
		svc := testSvc(t, &stubResolver{token: "tok"})
		rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rep.Message, "502") {
			t.Fatalf("message = %q", rep.Message)
		}
	})
	t.Run("decode", func(t *testing.T) {
		cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
				_, _ = w.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		svc := testSvc(t, &stubResolver{token: "tok"})
		rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rep.Message, "could not be decoded") {
			t.Fatalf("message = %q", rep.Message)
		}
	})
}

func TestCursorMissingPlanUsageUsesCLIUnavailable(t *testing.T) {
	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
			_, _ = w.Write(buildPeriodUsage(nil, nil, 0))
		case "/aiserver.v1.DashboardService/GetMe":
			_, _ = w.Write(encodeCursorInt32Field(2, 7))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Message != cursorUnavailableMessage || len(rep.Quotas) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func encodeCursorMe(userID, teamID int32, enterprise bool) []byte {
	out := append(encodeCursorInt32Field(2, userID), encodeCursorInt32Field(7, teamID)...)
	if enterprise {
		out = append(out, encodeCursorVarintField(9, 1)...)
	}
	return out
}

func encodeCursorMonthlyCycle(start, end int64) []byte {
	return append(encodeCursorVarintField(1, uint64(start)), encodeCursorVarintField(2, uint64(end))...)
}

func TestCursorEnterpriseSpendSuccessAndZero(t *testing.T) {
	start := int64(1_700_000_000_000)
	end := start + int64(30*24*time.Hour/time.Millisecond)
	now := time.UnixMilli(start + int64(10*24*time.Hour/time.Millisecond)).UTC()
	var gotBody []byte
	var gotPath string
	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
			_, _ = w.Write(buildPeriodUsage(nil, nil, 0))
		case "/aiserver.v1.DashboardService/GetPlanInfo":
			_, _ = w.Write(encodeCursorLenField(1, encodeCursorStringField(1, "Enterprise")))
		case "/aiserver.v1.DashboardService/GetMe":
			if len(body) != 0 {
				t.Errorf("GetMe body = %d bytes, want empty", len(body))
			}
			_, _ = w.Write(encodeCursorMe(42, 7, true))
		case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
			if len(body) != 0 {
				t.Errorf("monthly body = %d bytes, want empty", len(body))
			}
			_, _ = w.Write(encodeCursorMonthlyCycle(start, end))
		case "/aiserver.v1.DashboardService/GetAggregatedUsageEvents":
			gotPath = r.URL.Path
			gotBody = append([]byte(nil), body...)
			_, _ = w.Write(encodeCursorDoubleField(6, 251))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	svc := testSvc(t, &stubResolver{token: "tok"})
	svc.now = func() time.Time { return now }
	rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath == "" {
		t.Fatal("aggregated usage was not requested")
	}
	fields, ok := decodeCursorFields(gotBody)
	if !ok {
		t.Fatal("request did not decode")
	}
	if v, _, valid := cursorInt32(fields, 1); !valid || v != 7 {
		t.Fatalf("team_id = %d valid=%v", v, valid)
	}
	if v, _, valid := cursorInt64(fields, 2); !valid || v != start {
		t.Fatalf("start = %d", v)
	}
	if v, _, valid := cursorInt64(fields, 3); !valid || v != now.UnixMilli() {
		t.Fatalf("end = %d, want clamped now %d", v, now.UnixMilli())
	}
	if v, _, valid := cursorInt32(fields, 4); !valid || v != 42 {
		t.Fatalf("user_id = %d", v)
	}
	if rep.Plan != "Enterprise" || len(rep.Quotas) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	startDate := time.UnixMilli(start).UTC().Format("2006-01-02")
	endDate := time.UnixMilli(end).UTC().Format("2006-01-02")
	wantMsg := "Enterprise spend is $2.51 from " + startDate + " to " + endDate + " UTC."
	if rep.Message != wantMsg {
		t.Fatalf("message = %q, want %q", rep.Message, wantMsg)
	}

	t.Run("zero spend", func(t *testing.T) {
		cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
				_, _ = w.Write(buildPeriodUsage(nil, nil, 0))
			case "/aiserver.v1.DashboardService/GetMe":
				_, _ = w.Write(encodeCursorMe(42, 7, true))
			case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
				_, _ = w.Write(encodeCursorMonthlyCycle(start, end))
			case "/aiserver.v1.DashboardService/GetAggregatedUsageEvents":
				_, _ = w.Write(encodeCursorDoubleField(6, 0))
			default:
				w.WriteHeader(http.StatusOK)
			}
		})
		svc := testSvc(t, &stubResolver{token: "tok"})
		svc.now = func() time.Time { return now }
		rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Quotas) != 0 || !strings.Contains(rep.Message, "$0.00") {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("omitted zero", func(t *testing.T) {
		cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
				_, _ = w.Write(buildPeriodUsage(nil, nil, 0))
			case "/aiserver.v1.DashboardService/GetMe":
				_, _ = w.Write(encodeCursorMe(42, 7, true))
			case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
				_, _ = w.Write(encodeCursorMonthlyCycle(start, end))
			case "/aiserver.v1.DashboardService/GetAggregatedUsageEvents":
				_, _ = w.Write([]byte{})
			default:
				w.WriteHeader(http.StatusOK)
			}
		})
		svc := testSvc(t, &stubResolver{token: "tok"})
		svc.now = func() time.Time { return now }
		rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Quotas) != 0 || !strings.Contains(rep.Message, "$0.00") || strings.Contains(rep.Message, "not started") {
			t.Fatalf("report = %+v", rep)
		}
	})
}

func TestCursorEnterpriseSpendFailures(t *testing.T) {
	start := int64(1_700_000_000_000)
	end := start + int64(30*24*time.Hour/time.Millisecond)
	now := time.UnixMilli(start + int64(time.Hour/time.Millisecond)).UTC()
	cases := []struct {
		name string
		hit  func(w http.ResponseWriter, r *http.Request)
	}{
		{name: "missing ids", hit: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/aiserver.v1.DashboardService/GetMe" {
				_, _ = w.Write(encodeCursorVarintField(9, 1))
				return
			}
			w.WriteHeader(http.StatusOK)
		}},
		{name: "malformed cycle", hit: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aiserver.v1.DashboardService/GetMe":
				_, _ = w.Write(encodeCursorMe(1, 2, true))
			case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
				_, _ = w.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
			default:
				w.WriteHeader(http.StatusOK)
			}
		}},
		{name: "denied", hit: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aiserver.v1.DashboardService/GetMe":
				_, _ = w.Write(encodeCursorMe(1, 2, true))
			case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
				http.Error(w, "no", http.StatusForbidden)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}},
		{name: "partial failure", hit: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aiserver.v1.DashboardService/GetMe":
				_, _ = w.Write(encodeCursorMe(1, 2, true))
			case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
				_, _ = w.Write(encodeCursorMonthlyCycle(start, end))
			case "/aiserver.v1.DashboardService/GetAggregatedUsageEvents":
				http.Error(w, "no", http.StatusBadGateway)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
					_, _ = w.Write(buildPeriodUsage(nil, nil, 0))
					return
				}
				tc.hit(w, r)
			})
			svc := testSvc(t, &stubResolver{token: "tok"})
			svc.now = func() time.Time { return now }
			rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
			if err != nil {
				t.Fatal(err)
			}
			if rep.Message == "" || len(rep.Quotas) != 0 {
				t.Fatalf("report = %+v, want unavailable fallback", rep)
			}
		})
	}
}

func TestCursorEnterpriseSpendWindow(t *testing.T) {
	start := int64(1_700_000_000_000)
	end := start + int64(30*24*time.Hour/time.Millisecond)
	cases := []struct {
		name    string
		now     time.Time
		ask     bool
		started bool
	}{
		{name: "before", now: time.UnixMilli(start - 1).UTC(), started: false},
		{name: "start", now: time.UnixMilli(start).UTC(), started: true},
		{name: "mid", now: time.UnixMilli(start + 1).UTC(), ask: true, started: true},
		{name: "after", now: time.UnixMilli(end + 1).UTC(), ask: true, started: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
					_, _ = w.Write(buildPeriodUsage(nil, nil, 0))
				case "/aiserver.v1.DashboardService/GetMe":
					_, _ = w.Write(encodeCursorMe(1, 2, true))
				case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
					_, _ = w.Write(encodeCursorMonthlyCycle(start, end))
				case "/aiserver.v1.DashboardService/GetAggregatedUsageEvents":
					asked = true
					_, _ = w.Write(encodeCursorDoubleField(6, 100))
				default:
					w.WriteHeader(http.StatusOK)
				}
			})
			svc := testSvc(t, &stubResolver{token: "tok"})
			svc.now = func() time.Time { return tc.now }
			rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
			if err != nil {
				t.Fatal(err)
			}
			if asked != tc.ask {
				t.Fatalf("asked = %v, want %v", asked, tc.ask)
			}
			if len(rep.Quotas) != 0 {
				t.Fatalf("quotas = %+v", rep.Quotas)
			}
			if tc.started {
				if strings.Contains(rep.Message, "has not started") {
					t.Fatalf("message = %q, cycle has started", rep.Message)
				}
				wantAmount := "$0.00"
				if tc.ask {
					wantAmount = "$1.00"
				}
				if !strings.Contains(rep.Message, wantAmount) {
					t.Fatalf("message = %q, want %s", rep.Message, wantAmount)
				}
			}
			if !tc.started && !strings.Contains(rep.Message, "has not started") {
				t.Fatalf("message = %q", rep.Message)
			}
		})
	}
}

func TestCursorEnterpriseInvalidCostRejected(t *testing.T) {
	start := int64(1_700_000_000_000)
	end := start + 10_000
	nonFinite := func(bits uint64) []byte {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], bits)
		out := append([]byte{byte(6<<3 | 1)}, buf[:]...)
		return out
	}
	cases := []struct {
		name string
		body []byte
	}{
		{name: "varint", body: encodeCursorInt32Field(6, 250)},
		{name: "nan", body: nonFinite(0x7ff8000000000001)},
		{name: "inf", body: nonFinite(0x7ff0000000000000)},
		{name: "malformed", body: []byte{0xff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
					_, _ = w.Write(buildPeriodUsage(nil, nil, 0))
				case "/aiserver.v1.DashboardService/GetMe":
					_, _ = w.Write(encodeCursorMe(1, 2, true))
				case "/aiserver.v1.DashboardService/GetMonthlyBillingCycle":
					_, _ = w.Write(encodeCursorMonthlyCycle(start, end))
				case "/aiserver.v1.DashboardService/GetAggregatedUsageEvents":
					_, _ = w.Write(tc.body)
				default:
					w.WriteHeader(http.StatusOK)
				}
			})
			svc := testSvc(t, &stubResolver{token: "tok"})
			svc.now = func() time.Time { return time.UnixMilli(start + 1).UTC() }
			rep, err := svc.Fetch(context.Background(), cursorProvider("tok", "m1"))
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Quotas) != 0 || rep.Message != cursorEnterpriseUnavailable {
				t.Fatalf("report = %+v", rep)
			}
		})
	}
}

func TestCursorProviderBaseURLOverridesDashboard(t *testing.T) {
	var gotHost atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
			planUsage := append(encodeCursorInt32Field(2, 1), encodeCursorInt32Field(5, 10)...)
			_, _ = w.Write(buildPeriodUsage(planUsage, nil, 0))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	p := cursorProvider("tok", "m1")
	p.BaseURL = up.URL
	svc := testSvc(t, &stubResolver{token: "tok"})
	if _, err := svc.Fetch(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	host, _ := gotHost.Load().(string)
	if host == "" || !strings.Contains(up.URL, host) {
		t.Fatalf("host = %q url=%s", host, up.URL)
	}
}

func TestCursorIgnoresAgentAPI5Base(t *testing.T) {
	planUsage := append(encodeCursorInt32Field(2, 1), encodeCursorInt32Field(5, 10)...)
	periodBody := buildPeriodUsage(planUsage, nil, 0)
	var hits atomic.Int32
	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
			_, _ = w.Write(periodBody)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	p := cursorProvider("tok", "m1")
	p.BaseURL = "https://agent.api5.cursor.sh"
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Quotas) == 0 {
		t.Fatalf("should use CursorDashboardBaseURL, got %+v", rep)
	}
	if hits.Load() == 0 {
		t.Fatal("expected dashboard host hits")
	}
}

func TestCursorColonPrefixedTokenStrippedInHeaders(t *testing.T) {
	var gotAuth string
	cursorTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/aiserver.v1.DashboardService/GetCurrentPeriodUsage" {
			gotAuth = r.Header.Get("Authorization")
			planUsage := append(encodeCursorInt32Field(2, 1), encodeCursorInt32Field(5, 10)...)
			_, _ = w.Write(buildPeriodUsage(planUsage, nil, 0))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	svc := testSvc(t, &stubResolver{token: "sess::tok"})
	if _, err := svc.Fetch(context.Background(), cursorProvider("sess::tok", "m1")); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
}
