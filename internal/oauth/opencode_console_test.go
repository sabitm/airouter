package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/opencode"
)

func TestOpencodeConsoleDeviceWire(t *testing.T) {
	var mu sync.Mutex
	var codeBody map[string]any
	var tokenBodies []map[string]any
	tokenCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		switch r.URL.Path {
		case "/auth/device/code":
			mu.Lock()
			codeBody = got
			mu.Unlock()
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("device code content type = %q", r.Header.Get("Content-Type"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_code":"dev-secret","user_code":"USER","verification_uri_complete":"/device?user_code=USER","expires_in":60,"interval":1}`)
		case "/auth/device/token":
			mu.Lock()
			tokenBodies = append(tokenBodies, got)
			tokenCalls++
			n := tokenCalls
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
				return
			}
			if n == 2 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"slow_down"}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"access-secret","refresh_token":"refresh-secret","expires_in":600,"org_id":"org-b"}`)
		case "/api/user":
			if r.Header.Get("Authorization") != "Bearer access-secret" {
				t.Errorf("user auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"id":"user-1","email":"user@example.com"}`)
		case "/api/orgs":
			if r.Header.Get("Authorization") != "Bearer access-secret" {
				t.Errorf("orgs auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `[{"id":"org-a","name":"Alpha"},{"id":"org-b","name":"Beta"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	prevOrigin := opencode.ConsoleOrigin
	opencode.ConsoleOrigin = srv.URL
	t.Cleanup(func() { opencode.ConsoleOrigin = prevOrigin })
	var waits []time.Duration
	restoreWait := SetOpencodeConsoleWaitForTest(func(context.Context, time.Duration) error {
		return nil
	})
	t.Cleanup(restoreWait)
	// Record waits with a wrapper that still returns immediately.
	restoreWait()
	restoreWait = SetOpencodeConsoleWaitForTest(func(_ context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return nil
	})
	t.Cleanup(restoreWait)
	prevClient := opencodeConsoleHTTP
	opencodeConsoleHTTP = srv.Client()
	opencodeConsoleHTTP.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errRedirectRefused
	}
	t.Cleanup(func() { opencodeConsoleHTTP = prevClient })

	conn, err := NewOpencodeConsoleConnect()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if conn.VerificationURI() != srv.URL+"/device?user_code=USER" {
		t.Fatalf("verification URL = %q", conn.VerificationURI())
	}
	creds := waitOpencodeCreds(t, conn)
	if creds.OpencodeOrgID != "org-b" || creds.OpencodeOrgName != "Beta" || creds.Email != "user@example.com" {
		t.Fatalf("creds org/email = %+v", creds)
	}
	if creds.AccessToken != "access-secret" || !creds.OpencodeConsoleAuth {
		t.Fatalf("creds = %+v", creds)
	}
	mu.Lock()
	defer mu.Unlock()
	if codeBody["client_id"] != opencode.ConsoleClientID || codeBody["supports_org_scope"] != true {
		t.Fatalf("device code body = %#v", codeBody)
	}
	if len(tokenBodies) != 3 {
		t.Fatalf("token calls = %d", len(tokenBodies))
	}
	for _, body := range tokenBodies {
		if body["grant_type"] != opencodeDeviceGrant || body["client_id"] != opencode.ConsoleClientID || body["device_code"] != "dev-secret" {
			t.Fatalf("token body = %#v", body)
		}
	}
	if len(waits) < 3 || waits[2]-waits[1] != 5*time.Second {
		t.Fatalf("waits = %v, want slow_down to add 5s", waits)
	}
	if strings.Contains(conn.VerificationURI(), "dev-secret") {
		t.Fatal("verification URL contains device code")
	}
}

func TestOpencodeConsoleDeviceErrors(t *testing.T) {
	t.Run("relative and invalid verification URL", func(t *testing.T) {
		if got, err := resolveOpencodeVerificationURL("https://opencode.ai/console", "/device?user_code=USER", "dev-secret"); err != nil || got != "https://opencode.ai/device?user_code=USER" {
			t.Fatalf("origin-relative URL = %q, %v", got, err)
		}
		if got, err := resolveOpencodeVerificationURL("https://opencode.ai/console", "device?user_code=USER", "dev-secret"); err != nil || got != "https://opencode.ai/console/device?user_code=USER" {
			t.Fatalf("path-relative URL = %q, %v", got, err)
		}
		if _, err := resolveOpencodeVerificationURL("https://opencode.ai/console", "javascript:alert(1)", "dev"); err == nil {
			t.Fatal("javascript URL was accepted")
		}
		if _, err := resolveOpencodeVerificationURL("https://opencode.ai/console", "/device?device_code=dev-secret", "dev-secret"); err == nil || strings.Contains(err.Error(), "dev-secret") {
			t.Fatalf("device code URL error = %v", err)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/device/code" {
				_, _ = io.WriteString(w, `{"device_code":"dev-secret","user_code":"USER","verification_uri_complete":"/device","expires_in":60,"interval":5}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
		}))
		defer srv.Close()
		restore := useOpencodeConsoleServer(t, srv)
		defer restore()
		restoreWait := SetOpencodeConsoleWaitForTest(func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		})
		defer restoreWait()
		conn, err := NewOpencodeConsoleConnect()
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		_, err, done := waitOpencodeDone(t, conn)
		if !done || err == nil || strings.Contains(err.Error(), "dev-secret") {
			t.Fatalf("cancel result err=%v done=%v", err, done)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/device/token" {
				hits++
			}
			_, _ = io.WriteString(w, `{"device_code":"dev-secret","user_code":"USER","verification_uri_complete":"/device","expires_in":30,"interval":1}`)
		}))
		defer srv.Close()
		restore := useOpencodeConsoleServer(t, srv)
		defer restore()
		start := time.Unix(1_000, 0)
		calls := 0
		prevNow := opencodeConsoleNow
		opencodeConsoleNow = func() time.Time {
			calls++
			if calls > 1 {
				return start.Add(time.Hour)
			}
			return start
		}
		t.Cleanup(func() { opencodeConsoleNow = prevNow })
		restoreWait := SetOpencodeConsoleWaitForTest(func(context.Context, time.Duration) error { return nil })
		defer restoreWait()
		conn, err := NewOpencodeConsoleConnect()
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err, done := waitOpencodeDone(t, conn)
		if !done || err == nil || !strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "dev-secret") {
			t.Fatalf("expiry err=%v", err)
		}
		if hits != 0 {
			t.Fatalf("expired flow polled %d times", hits)
		}
	})

	t.Run("terminal error hides device code", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/device/code" {
				_, _ = io.WriteString(w, `{"device_code":"dev-secret","user_code":"USER","verification_uri_complete":"/device","expires_in":60,"interval":1}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"access_denied"}`)
		}))
		defer srv.Close()
		restore := useOpencodeConsoleServer(t, srv)
		defer restore()
		restoreWait := SetOpencodeConsoleWaitForTest(func(context.Context, time.Duration) error { return nil })
		defer restoreWait()
		conn, err := NewOpencodeConsoleConnect()
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err, _ = waitOpencodeDone(t, conn)
		if err == nil || !strings.Contains(err.Error(), "access_denied") || strings.Contains(err.Error(), "dev-secret") {
			t.Fatalf("terminal err=%v", err)
		}
	})
}

func TestOpencodeConsoleOrgSelection(t *testing.T) {
	t.Run("org id must match and is not substituted", func(t *testing.T) {
		creds, err := runOpencodeDevice(t, `{"access_token":"a","refresh_token":"r","expires_in":60,"org_id":"missing"}`, `[{"id":"only","name":"Only"}]`)
		if err == nil || creds != nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err=%v creds=%v", err, creds)
		}
		if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "refresh") {
			t.Fatalf("error leaked token or org id: %v", err)
		}
	})

	t.Run("sole workspace when org id is absent", func(t *testing.T) {
		creds, err := runOpencodeDevice(t, `{"access_token":"a","refresh_token":"r","expires_in":60}`, `[{"id":"only","name":"Only"}]`)
		if err != nil || creds.OpencodeOrgID != "only" || creds.OpencodeOrgName != "Only" {
			t.Fatalf("creds=%+v err=%v", creds, err)
		}
	})

	t.Run("no workspace fails", func(t *testing.T) {
		_, err := runOpencodeDevice(t, `{"access_token":"a","refresh_token":"r","expires_in":60}`, `[]`)
		if err == nil || !strings.Contains(err.Error(), "no workspace") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("choice is server validated", func(t *testing.T) {
		conn := startOpencodeChoice(t)
		if err := conn.ChooseWorkspace("not-listed"); err == nil || strings.Contains(err.Error(), "access") {
			t.Fatalf("mismatch err=%v", err)
		}
		phase, _, _, orgs := conn.Status()
		if phase != "choice" || len(orgs) != 2 {
			t.Fatalf("phase=%s orgs=%v", phase, orgs)
		}
		if _, _, done := conn.Result(); done {
			t.Fatal("mismatch completed the grant")
		}
		if err := conn.ChooseWorkspace("org-b"); err != nil {
			t.Fatal(err)
		}
		creds, err, done := conn.Result()
		if !done || err != nil || creds.OpencodeOrgID != "org-b" || creds.OpencodeOrgName != "Beta" {
			t.Fatalf("choice creds=%+v err=%v done=%v", creds, err, done)
		}
	})
}

func TestOpencodeConsoleRefresh(t *testing.T) {
	evilHits := 0
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()
	var body map[string]string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/auth/device/token" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":120}`)
	}))
	defer srv.Close()
	restore := useOpencodeConsoleServer(t, srv)
	defer restore()
	creds := &domain.OAuthCreds{
		OpencodeConsoleAuth: true,
		AccessToken:         "old-access",
		RefreshToken:        "old-refresh",
		OpencodeOrgID:       "org-1",
		OpencodeOrgName:     "Workspace",
		TokenURL:            evil.URL + "/steal",
		ClientID:            "attacker",
	}
	if err := refreshOpencodeConsole(context.Background(), creds, time.Unix(1_000, 0)); err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "new-access" || creds.RefreshToken != "new-refresh" || creds.ExpiresAt != 1120 {
		t.Fatalf("rotated = %+v", creds)
	}
	if creds.OpencodeOrgID != "org-1" || creds.OpencodeOrgName != "Workspace" {
		t.Fatalf("workspace changed: %+v", creds)
	}
	if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" || body["client_id"] != opencode.ConsoleClientID {
		t.Fatalf("refresh body = %#v", body)
	}
	if auth != "" {
		t.Fatalf("refresh sent Authorization %q", auth)
	}
	if evilHits != 0 {
		t.Fatalf("refresh token was sent to %d invalid destinations", evilHits)
	}

	t.Run("invalid grant", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
		}))
		defer srv.Close()
		restore := useOpencodeConsoleServer(t, srv)
		defer restore()
		c := consoleBasis()
		err := refreshOpencodeConsole(context.Background(), c, time.Now())
		if !IsInvalidGrant(err) || strings.Contains(err.Error(), "old-refresh") {
			t.Fatalf("err=%v", err)
		}
		if c.AccessToken != "old-access" {
			t.Fatalf("invalid grant mutated creds: %+v", c)
		}
	})

	t.Run("org mismatch does not switch workspace", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"access_token":"other","refresh_token":"other-refresh","expires_in":60,"org_id":"org-2"}`)
		}))
		defer srv.Close()
		restore := useOpencodeConsoleServer(t, srv)
		defer restore()
		c := consoleBasis()
		err := refreshOpencodeConsole(context.Background(), c, time.Now())
		if !IsInvalidGrant(err) {
			t.Fatalf("err=%v", err)
		}
		if c.OpencodeOrgID != "org-1" || c.AccessToken != "old-access" || c.RefreshToken != "old-refresh" {
			t.Fatalf("mismatch persisted: %+v", c)
		}
	})

	t.Run("redirect is refused", func(t *testing.T) {
		hits := 0
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
		}))
		defer srv.Close()
		restore := useOpencodeConsoleServer(t, srv)
		defer restore()
		c := consoleBasis()
		err := refreshOpencodeConsole(context.Background(), c, time.Now())
		if err == nil || hits != 1 || evilHits != 0 {
			t.Fatalf("redirect err=%v hits=%d evil=%d", err, hits, evilHits)
		}
		if c.AccessToken != "old-access" {
			t.Fatalf("redirect mutated creds")
		}
	})
}

func TestOpencodeConsoleGuardedRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":90}`)
	}))
	defer srv.Close()
	restore := useOpencodeConsoleServer(t, srv)
	defer restore()
	basis := consoleBasis()
	st := newFakeStore()
	st.creds[7] = cloneConsoleCreds(basis)
	st.setConnection(7, domain.AuthOAuth, domain.ProtocolOpencode)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(2_000, 0) }

	got, err := svc.RefreshAndPersist(context.Background(), 7, cloneConsoleCreds(basis))
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.OpencodeOrgID != "org-1" || got.OpencodeOrgName != "Workspace" {
		t.Fatalf("persisted = %+v", got)
	}

	t.Run("reconnect wins over late refresh", func(t *testing.T) {
		st := newFakeStore()
		st.creds[7] = cloneConsoleCreds(basis)
		st.setConnection(7, domain.AuthOAuth, domain.ProtocolOpencode)
		blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			replaced := cloneConsoleCreds(basis)
			replaced.AccessToken = "reconnected"
			replaced.RefreshToken = "reconnected-refresh"
			st.creds[7] = replaced
			_, _ = io.WriteString(w, `{"access_token":"late","refresh_token":"late-refresh","expires_in":90}`)
		}))
		defer blocked.Close()
		restore := useOpencodeConsoleServer(t, blocked)
		defer restore()
		svc := New(st)
		_, err := svc.RefreshAndPersist(context.Background(), 7, cloneConsoleCreds(basis))
		if !IsCredentialConflict(err) {
			t.Fatalf("err=%v", err)
		}
		if st.creds[7].AccessToken != "reconnected" || st.creds[7].RefreshToken != "reconnected-refresh" {
			t.Fatalf("late refresh overwrote reconnect: %+v", st.creds[7])
		}
	})

	t.Run("auth switch does not receive rotation", func(t *testing.T) {
		st := newFakeStore()
		st.creds[7] = cloneConsoleCreds(basis)
		st.setConnection(7, domain.AuthAPIKey, domain.ProtocolOpencode)
		hits := 0
		blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			_, _ = io.WriteString(w, `{"access_token":"late","refresh_token":"late-refresh","expires_in":90}`)
		}))
		defer blocked.Close()
		restore := useOpencodeConsoleServer(t, blocked)
		defer restore()
		svc := New(st)
		_, err := svc.RefreshAndPersist(context.Background(), 7, cloneConsoleCreds(basis))
		if !IsCredentialConflict(err) || hits != 0 {
			t.Fatalf("err=%v hits=%d", err, hits)
		}
		if st.creds[7].AccessToken != "old-access" {
			t.Fatalf("switch received rotation: %+v", st.creds[7])
		}
	})

	t.Run("protocol switch does not receive rotation", func(t *testing.T) {
		st := newFakeStore()
		st.creds[7] = cloneConsoleCreds(basis)
		st.setConnection(7, domain.AuthOAuth, domain.ProtocolOpenAI)
		svc := New(st)
		_, err := svc.RefreshAndPersist(context.Background(), 7, cloneConsoleCreds(basis))
		if !IsCredentialConflict(err) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestOpencodeConsoleResolveRejectsConnectionMismatch(t *testing.T) {
	console := consoleBasis()
	otherWorkspace := cloneConsoleCreds(console)
	otherWorkspace.OpencodeOrgID = "org-2"
	generic := &domain.OAuthCreds{AccessToken: "generic", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	for _, tc := range []struct {
		name       string
		snapshot   *domain.OAuthCreds
		protocol   domain.Protocol
		stored     *domain.OAuthCreds
		liveMethod domain.AuthMethod
		liveProto  domain.Protocol
	}{
		{"generic snapshot cannot resolve Console", generic, domain.ProtocolOpenAI, console, domain.AuthOAuth, domain.ProtocolOpencode},
		{"Console snapshot after key switch", console, domain.ProtocolOpencode, nil, domain.AuthAPIKey, domain.ProtocolOpencode},
		{"Console snapshot after protocol switch", console, domain.ProtocolOpencode, generic, domain.AuthOAuth, domain.ProtocolOpenAI},
		{"Console snapshot after workspace switch", console, domain.ProtocolOpencode, otherWorkspace, domain.AuthOAuth, domain.ProtocolOpencode},
		{"unmarked OpenCode OAuth", generic, domain.ProtocolOpencode, generic, domain.AuthOAuth, domain.ProtocolOpencode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.setConnection(7, tc.liveMethod, tc.liveProto)
			if tc.stored != nil {
				st.creds[7] = tc.stored
			}
			p := &domain.Provider{ID: 7, BaseURL: opencode.GoBaseURL, Protocol: tc.protocol, AuthMethod: domain.AuthOAuth, OAuthCreds: tc.snapshot}
			for _, force := range []bool{false, true} {
				got, err := New(st).ResolveCreds(t.Context(), p, force)
				if got != nil || !IsCredentialConflict(err) || st.writes.Load() != 0 {
					t.Fatalf("force=%v resolved=%v err=%v writes=%d", force, got != nil, err, st.writes.Load())
				}
			}
		})
	}
}

func TestOpencodeConsoleRefreshCoalescesAllPaths(t *testing.T) {
	basis := consoleBasis()
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		var grant map[string]string
		if err := json.NewDecoder(r.Body).Decode(&grant); err != nil || grant["refresh_token"] != "old-refresh" {
			t.Errorf("unexpected refresh grant: %v", err)
		}
		<-release
		_, _ = io.WriteString(w, `{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":600}`)
	}))
	defer srv.Close()
	restore := useOpencodeConsoleServer(t, srv)
	defer restore()
	defer unblock()
	st := newFakeStore()
	st.creds[7] = basis
	st.setConnection(7, domain.AuthOAuth, domain.ProtocolOpencode)
	svc := New(st)
	joined := make(chan struct{}, 3)
	svc.testOnJoin = func() { joined <- struct{}{} }
	p, err := st.GetProvider(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	defer func() { unblock(); wg.Wait() }()
	for _, path := range []func() (*domain.OAuthCreds, error){
		func() (*domain.OAuthCreds, error) { return svc.ResolveCreds(ctx, p, false) },
		func() (*domain.OAuthCreds, error) { return svc.ResolveCreds(ctx, p, true) },
		func() (*domain.OAuthCreds, error) { return svc.RefreshAndPersist(ctx, p.ID, basis) },
	} {
		wg.Go(func() {
			got, err := path()
			if err != nil || got == nil || got.AccessToken != "rotated" || got.OpencodeOrgID != "org-1" {
				t.Errorf("refresh returned wrong snapshot: %v", err)
			}
		})
	}
	for range 3 {
		select {
		case <-joined:
		case <-ctx.Done():
			t.Fatal("not all refresh paths joined")
		}
	}
	unblock()
	wg.Wait()

	// A delayed auth failure for the old token reuses the rotation.
	got, err := svc.ResolveCreds(ctx, p, true)
	if err != nil || got == nil || got.AccessToken != "rotated" {
		t.Fatalf("delayed forced resolution returned wrong snapshot: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 || st.writes.Load() != 1 {
		t.Fatalf("refresh HTTP calls=%d writes=%d, want one each", hits, st.writes.Load())
	}
}

func consoleBasis() *domain.OAuthCreds {
	return &domain.OAuthCreds{
		OpencodeConsoleAuth: true,
		AccessToken:         "old-access",
		RefreshToken:        "old-refresh",
		ExpiresAt:           1,
		OpencodeOrgID:       "org-1",
		OpencodeOrgName:     "Workspace",
		Email:               "user@example.com",
	}
}

func cloneConsoleCreds(c *domain.OAuthCreds) *domain.OAuthCreds {
	cp := *c
	return &cp
}

var errRedirectRefused = http.ErrUseLastResponse

func useOpencodeConsoleServer(t *testing.T, srv *httptest.Server) func() {
	t.Helper()
	prevOrigin := opencode.ConsoleOrigin
	prevClient := opencodeConsoleHTTP
	opencode.ConsoleOrigin = srv.URL
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("oauth: redirect refused")
	}
	opencodeConsoleHTTP = client
	return func() {
		opencode.ConsoleOrigin = prevOrigin
		opencodeConsoleHTTP = prevClient
	}
}

func runOpencodeDevice(t *testing.T, tokenJSON, orgsJSON string) (*domain.OAuthCreds, error) {
	t.Helper()
	conn := startOpencodeDevice(t, tokenJSON, orgsJSON)
	creds, err, done := waitOpencodeDone(t, conn)
	if !done {
		t.Fatal("device flow did not finish")
	}
	return creds, err
}

func startOpencodeChoice(t *testing.T) *OpencodeConsoleConnect {
	t.Helper()
	conn := startOpencodeDevice(t, `{"access_token":"a","refresh_token":"r","expires_in":60}`, `[{"id":"org-a","name":"Alpha"},{"id":"org-b","name":"Beta"}]`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		phase, _, _, orgs := conn.Status()
		if phase == "choice" && len(orgs) == 2 {
			return conn
		}
		if _, _, done := conn.Result(); done {
			t.Fatal("choice flow finished early")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for workspace choice")
	return nil
}

func startOpencodeDevice(t *testing.T, tokenJSON, orgsJSON string) *OpencodeConsoleConnect {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/device/code":
			_, _ = io.WriteString(w, `{"device_code":"dev","user_code":"USER","verification_uri_complete":"/ok","expires_in":60,"interval":1}`)
		case "/auth/device/token":
			_, _ = io.WriteString(w, tokenJSON)
		case "/api/user":
			_, _ = io.WriteString(w, `{"id":"user","email":"user@example.com"}`)
		case "/api/orgs":
			_, _ = io.WriteString(w, orgsJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	restore := useOpencodeConsoleServer(t, srv)
	t.Cleanup(restore)
	restoreWait := SetOpencodeConsoleWaitForTest(func(context.Context, time.Duration) error { return nil })
	t.Cleanup(restoreWait)
	conn, err := NewOpencodeConsoleConnect()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return conn
}

func waitOpencodeCreds(t *testing.T, conn *OpencodeConsoleConnect) *domain.OAuthCreds {
	t.Helper()
	creds, err, done := waitOpencodeDone(t, conn)
	if !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	return creds
}

func waitOpencodeDone(t *testing.T, conn *OpencodeConsoleConnect) (*domain.OAuthCreds, error, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		creds, err, done := conn.Result()
		if done {
			return creds, err, true
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for device flow")
	return nil, nil, false
}
