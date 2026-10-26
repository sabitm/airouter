package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func qoderUsageServer(t *testing.T, body map[string]any) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(up.Close)
	prev := QoderUsageURL
	QoderUsageURL = up.URL
	t.Cleanup(func() { QoderUsageURL = prev })
	return up
}

func qoderQuotaNames(t *testing.T, rep *Report) []Quota {
	t.Helper()
	return rep.Quotas
}

// TestFetchQoderNeverExpiresSentinel covers the account-level "never expires"
// reset value: the sentinel must never render as a date.
func TestFetchQoderNeverExpiresSentinel(t *testing.T) {
	for _, sentinel := range []any{float64(253402214400000), "253402214400000"} {
		qoderUsageServer(t, map[string]any{
			"userQuota": map[string]any{"total": 100, "used": 10, "remaining": 90, "unit": "credits"},
			"expiresAt": sentinel,
		})
		svc := testSvc(t, &stubResolver{token: "tok"})
		rep, err := svc.Fetch(context.Background(), oauthProvider(1, "qoder", "tok"))
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Quotas) != 1 {
			t.Fatalf("quotas = %+v", rep.Quotas)
		}
		q := rep.Quotas[0]
		if !q.NeverExpires {
			t.Fatalf("sentinel %v: NeverExpires = false", sentinel)
		}
		if q.ResetAt != nil {
			t.Fatalf("sentinel %v: ResetAt = %v, want nil", sentinel, q.ResetAt)
		}
	}
}

// TestFetchQoderFiniteReset keeps real reset times as dates.
func TestFetchQoderFiniteReset(t *testing.T) {
	qoderUsageServer(t, map[string]any{
		"userQuota": map[string]any{"total": 100, "used": 10, "remaining": 90, "unit": "credits"},
		"expiresAt": 1_800_000_000_000,
	})
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, _ := svc.Fetch(context.Background(), oauthProvider(2, "qoder", "tok"))
	if len(rep.Quotas) != 1 {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
	if rep.Quotas[0].NeverExpires {
		t.Fatal("finite reset marked never expires")
	}
	if rep.Quotas[0].ResetAt == nil {
		t.Fatal("finite reset missing")
	}
}

// TestFetchQoderAddOnQuota covers the add-on bucket gating and its unknown
// expiry: positive totals appear, zero and missing never do, and neither the
// account sentinel nor a fabricated reset leaks onto the add-on row.
func TestFetchQoderAddOnQuota(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		want    []string
		addOnNE bool
	}{
		{
			name: "positive add-on",
			body: map[string]any{
				"userQuota":  map[string]any{"total": 100, "used": 10, "remaining": 90},
				"addOnQuota": map[string]any{"total": 2200, "used": 100, "remaining": 2100, "unit": "credits"},
				"expiresAt":  float64(253402214400000),
			},
			want: []string{"Personal", "Add-on"},
		},
		{
			name: "zero add-on omitted",
			body: map[string]any{
				"userQuota":  map[string]any{"total": 100, "used": 10, "remaining": 90},
				"addOnQuota": map[string]any{"total": 0, "used": 0, "remaining": 0},
				"expiresAt":  float64(1_800_000_000_000),
			},
			want: []string{"Personal"},
		},
		{
			name: "absent add-on omitted",
			body: map[string]any{
				"userQuota": map[string]any{"total": 100, "used": 10, "remaining": 90},
				"expiresAt": float64(1_800_000_000_000),
			},
			want: []string{"Personal"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qoderUsageServer(t, tc.body)
			svc := testSvc(t, &stubResolver{token: "tok"})
			rep, err := svc.Fetch(context.Background(), oauthProvider(3, "qoder", "tok"))
			if err != nil {
				t.Fatal(err)
			}
			quotas := qoderQuotaNames(t, rep)
			if len(quotas) != len(tc.want) {
				t.Fatalf("quotas = %+v, want %v", quotas, tc.want)
			}
			for i, want := range tc.want {
				if quotas[i].Name != want {
					t.Fatalf("quota %d = %q, want %q", i, quotas[i].Name, want)
				}
			}
			for _, q := range quotas {
				if q.Name != "Add-on" {
					continue
				}
				if q.NeverExpires || q.ResetAt != nil {
					t.Fatalf("add-on quota = %+v, want unknown expiry (nil reset, no sentinel)", q)
				}
				if q.Total != 2200 || q.Remaining != 2100 {
					t.Fatalf("add-on quota = %+v", q)
				}
			}
		})
	}
}

// TestFetchQoderAddOnExpiryNotFromAccountSentinel ensures a finite account
// reset never becomes the add-on row's reset either.
func TestFetchQoderAddOnExpiryNotFromAccountSentinel(t *testing.T) {
	qoderUsageServer(t, map[string]any{
		"userQuota":  map[string]any{"total": 100, "used": 10, "remaining": 90},
		"addOnQuota": map[string]any{"total": 500, "used": 0, "remaining": 500},
		"expiresAt":  float64(1_800_000_000_000),
	})
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, _ := svc.Fetch(context.Background(), oauthProvider(4, "qoder", "tok"))
	var addOn *Quota
	for i := range rep.Quotas {
		if rep.Quotas[i].Name == "Add-on" {
			addOn = &rep.Quotas[i]
		}
	}
	if addOn == nil {
		t.Fatalf("no add-on row in %+v", rep.Quotas)
	}
	if addOn.ResetAt != nil || addOn.NeverExpires {
		t.Fatalf("add-on = %+v, want unset expiry", addOn)
	}
}

// TestFetchQoderOrganizationNeverExpires keeps the org row on the account
// expiry signal.
func TestFetchQoderOrganizationNeverExpires(t *testing.T) {
	qoderUsageServer(t, map[string]any{
		"userQuota":          map[string]any{"total": 100, "used": 10, "remaining": 90},
		"orgResourcePackage": map[string]any{"total": 200, "used": 20, "remaining": 180},
		"expiresAt":          "253402214400000",
	})
	svc := testSvc(t, &stubResolver{token: "tok"})
	rep, _ := svc.Fetch(context.Background(), oauthProvider(5, "qoder", "tok"))
	if len(rep.Quotas) != 2 {
		t.Fatalf("quotas = %+v", rep.Quotas)
	}
	for _, q := range rep.Quotas {
		if !q.NeverExpires || q.ResetAt != nil {
			t.Fatalf("quota %q = %+v, want never expires", q.Name, q)
		}
	}
}
