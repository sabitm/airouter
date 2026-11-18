package usage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"airouter/internal/crypto"
	"airouter/internal/oauth"
	"airouter/internal/proxy/opencode"
	"airouter/internal/store"
)

// The rejected token is the snapshot ResolveCreds just returned. The retry must
// pass that snapshot so the shared resolver rotates it once. The original
// provider snapshot is already stale and must not be forced again.
func TestOpenCodeGoRetryRefreshesPairedSnapshotOnce(t *testing.T) {
	var tokenHits atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/device/token" {
			http.NotFound(w, r)
			return
		}
		n := tokenHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","expires_in":600}`, n, n)
	}))
	t.Cleanup(tokenSrv.Close)
	prevOrigin := opencode.ConsoleOrigin
	opencode.ConsoleOrigin = tokenSrv.URL
	t.Cleanup(func() { opencode.ConsoleOrigin = prevOrigin })
	restore := oauth.SetOpencodeConsoleClientForTest(tokenSrv.Client())
	t.Cleanup(restore)

	var mu sync.Mutex
	var auths []string
	usageClient := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != opencode.GoUsageURL {
			t.Errorf("usage url = %s", r.URL.String())
		}
		if r.Header.Get("x-opencode-org-id") != "org-1" {
			t.Errorf("org = %q", r.Header.Get("x-opencode-org-id"))
		}
		auth := r.Header.Get("Authorization")
		mu.Lock()
		auths = append(auths, auth)
		mu.Unlock()
		if auth == "Bearer access-1" {
			return jsonResp(r, http.StatusUnauthorized, `{"detail":"secret-body"}`), nil
		}
		if auth == "Bearer access-2" {
			return jsonResp(r, http.StatusOK, openCodeUsageBody(10, 20, 30, "ok", openCodeReset)), nil
		}
		t.Errorf("unexpected usage auth %q", auth)
		return jsonResp(r, http.StatusUnauthorized, `{}`), nil
	})}

	cipher, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"), cipher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := consoleUsageProvider(0, "user-1", "org-1", "access-0", "refresh-0")
	p.OAuthCreds.ExpiresAt = 1
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	svc := NewService(oauth.New(st), nil, usageClient)
	rep, err := svc.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Message != "" || len(rep.Quotas) != 3 {
		t.Fatalf("rep = %+v", rep)
	}
	if strings.Contains(rep.Plan, "org-1") || strings.Contains(rep.Plan, "access-") || strings.Contains(rep.Message, "secret-body") {
		t.Fatalf("report leaked: %+v", rep)
	}
	if tokenHits.Load() != 2 {
		t.Fatalf("token refreshes = %d, want 2", tokenHits.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "Bearer access-1" || auths[1] != "Bearer access-2" {
		t.Fatalf("usage auths = %#v", auths)
	}
}
