package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/opencode"
)

func TestGrokBuildDevicePendingSlowDownAndCancel(t *testing.T) {
	var mu sync.Mutex
	var polls int
	var grants []string
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if r.URL.String() == grokbuild.DeviceCodeURL {
			if form.Get("client_id") != grokbuild.ClientID || form.Get("scope") != grokbuild.Scopes || form.Get("referrer") != grokbuild.Referrer {
				t.Errorf("device form = %v", form)
			}
			return jsonResponse(http.StatusOK, map[string]any{
				"device_code": "device-secret", "user_code": "ABCD-EFGH",
				"verification_uri": "https://auth.x.ai/oauth/device",
				"expires_in":       600, "interval": 1,
			}), nil
		}
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		grants = append(grants, form.Get("grant_type"))
		switch n {
		case 1:
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "authorization_pending"}), nil
		case 2:
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "slow_down"}), nil
		default:
			t.Errorf("poll continued after slow_down setup: %d", n)
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "authorization_pending"}), nil
		}
	}))
	var waits []time.Duration
	prev := grokBuildWait
	grokBuildWait = func(ctx context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) >= 3 {
			return false
		}
		return ctx.Err() == nil
	}
	t.Cleanup(func() { grokBuildWait = prev })

	conn, err := NewGrokBuildConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if conn.UserCode() != "ABCD-EFGH" || !strings.HasPrefix(conn.VerificationURI(), "https://auth.x.ai/") {
		t.Fatalf("public fields = %s %s", conn.UserCode(), conn.VerificationURI())
	}
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, done := conn.Result()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err, done := conn.Result()
	if !done || err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("result = %v done=%v", err, done)
	}
	if len(waits) < 3 || waits[2] != waits[0]+grokBuildSlowDown {
		t.Fatalf("intervals = %v, want slow_down added to the server interval", waits)
	}
	if grants[0] != deviceGrantType {
		t.Fatalf("grant = %s", grants[0])
	}
}

func TestGrokBuildRefreshRotatesToken(t *testing.T) {
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != grokbuild.TokenURL {
			t.Fatalf("url = %s", r.URL.String())
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" {
			t.Fatalf("form = %v", form)
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 60,
		}), nil
	}))
	creds := &domain.OAuthCreds{
		GrokBuildAuth: true, ClientID: grokbuild.ClientID, TokenURL: grokbuild.TokenURL,
		AccessToken: "old-access", RefreshToken: "old-refresh",
	}
	if err := refreshGrokBuild(context.Background(), creds, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "new-access" || creds.RefreshToken != "new-refresh" || creds.ExpiresAt != 1060 {
		t.Fatalf("rotated = %+v", creds)
	}
	xai := &domain.OAuthCreds{Preset: "xai", ClientID: grokbuild.ClientID, RefreshToken: "xai-refresh", TokenURL: grokbuild.TokenURL}
	if err := refreshGrokBuild(context.Background(), xai, time.Now()); err == nil {
		t.Fatal("xAI credentials used the Grok Build refresh")
	}
}

func TestGrokBuildRejectsForeignVerificationURI(t *testing.T) {
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, map[string]any{
			"device_code": "d", "user_code": "ABCD",
			"verification_uri": "https://evil.example/phish", "expires_in": 600, "interval": 5,
		}), nil
	}))
	if _, err := NewGrokBuildConnect(context.Background()); err == nil {
		t.Fatal("accepted a third-party verification URL")
	}
}

func SetGrokBuildTestTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	grokbuild.SetTestTransport(rt)
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
}

func jsonResponse(status int, doc any) *http.Response {
	raw, _ := json.Marshal(doc)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}
}

func TestGrokBuildDenial(t *testing.T) {
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/device/code") {
			return jsonResponse(http.StatusOK, map[string]any{
				"device_code": "d", "user_code": "WXYZ", "verification_uri": "https://auth.x.ai/oauth/device",
				"expires_in": 600, "interval": 1,
			}), nil
		}
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "access_denied"}), nil
	}))
	prev := grokBuildWait
	grokBuildWait = func(context.Context, time.Duration) bool { return true }
	t.Cleanup(func() { grokBuildWait = prev })
	conn, err := NewGrokBuildConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, done := conn.Result()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err, done := conn.Result()
	if !done || err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("result = %v", err)
	}
}

func TestGrokBuildRefreshRejectsReconnectDuringExchange(t *testing.T) {
	entered := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	var hits atomic.Int32
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != grokbuild.TokenURL {
			t.Errorf("refresh left the official token host: %s", r.URL.String())
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_request"}), nil
		}
		hits.Add(1)
		close(entered)
		<-hold
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 600,
		}), nil
	}))
	st := newFakeStore()
	basis := grokBuildServiceCreds("old-access", "old-refresh", "conn-1", "user-1")
	basis.ExpiresAt = 1
	st.creds[7] = basis
	st.setConnection(7, domain.AuthOAuth, domain.ProtocolGrokBuild)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	p, err := st.GetProvider(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, refreshErr := svc.ResolveCreds(ctx, p, true)
		errCh <- refreshErr
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("refresh did not reach the token exchange")
	}
	replaced := grokBuildServiceCreds("reconnected-access", "reconnected-refresh", "conn-2", "user-1")
	st.creds[7] = replaced
	release()
	select {
	case err := <-errCh:
		if !IsCredentialConflict(err) {
			t.Fatalf("exchange-time reconnect err = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("refresh did not finish")
	}
	got := st.creds[7]
	if got.AccessToken != "reconnected-access" || got.RefreshToken != "reconnected-refresh" || got.GrokBuildConnection != "conn-2" {
		t.Fatalf("reconnect row changed: %+v", got)
	}
	if hits.Load() != 1 || st.writes.Load() != 0 {
		t.Fatalf("hits=%d writes=%d", hits.Load(), st.writes.Load())
	}
}

func TestGrokBuildRefreshCoalescesAndRejectsReconnect(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var hits atomic.Int32
	var submitted atomic.Value
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != grokbuild.TokenURL {
			t.Errorf("refresh left the official token host: %s", r.URL.String())
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_request"}), nil
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		submitted.Store(form.Get("refresh_token"))
		hits.Add(1)
		<-release
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 600,
		}), nil
	}))
	st := newFakeStore()
	basis := grokBuildServiceCreds("old-access", "old-refresh", "conn-1", "user-1")
	basis.ExpiresAt = 1
	st.creds[7] = basis
	st.setConnection(7, domain.AuthOAuth, domain.ProtocolGrokBuild)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	joined := make(chan struct{}, 4)
	svc.testOnJoin = func() { joined <- struct{}{} }
	p, err := st.GetProvider(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	defer func() { unblock(); wg.Wait() }()
	for _, path := range []func() (*domain.OAuthCreds, error){
		func() (*domain.OAuthCreds, error) { return svc.ResolveCreds(ctx, p, false) },
		func() (*domain.OAuthCreds, error) { return svc.ResolveCreds(ctx, p, true) },
		func() (*domain.OAuthCreds, error) { return svc.RefreshAndPersist(ctx, p.ID, cloneGrokBuild(basis)) },
	} {
		wg.Go(func() {
			got, err := path()
			if err != nil || got == nil || got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" || got.GrokBuildConnection != "conn-1" {
				t.Errorf("refresh snapshot = %+v err=%v", got, err)
			}
		})
	}
	for range 3 {
		select {
		case <-joined:
		case <-ctx.Done():
			t.Fatal("refresh paths did not share one exchange")
		}
	}
	unblock()
	wg.Wait()
	reused, err := svc.ResolveCreds(ctx, p, true)
	if err != nil || reused == nil || reused.AccessToken != "rotated-access" {
		t.Fatalf("late forced refresh = %+v err=%v", reused, err)
	}
	if hits.Load() != 1 || st.writes.Load() != 1 {
		t.Fatalf("hits=%d writes=%d", hits.Load(), st.writes.Load())
	}
	if submitted.Load() != "old-refresh" {
		t.Fatalf("submitted grant = %v", submitted.Load())
	}
	persisted := st.creds[7]
	if persisted.RefreshToken != "rotated-refresh" || persisted.GrokBuildConnection != "conn-1" {
		t.Fatalf("persisted = %+v", persisted)
	}
	usable, err := svc.ResolveCreds(ctx, &domain.Provider{
		ID: 7, BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, OAuthCreds: cloneGrokBuild(persisted),
	}, false)
	if err != nil || usable.AccessToken != "rotated-access" {
		t.Fatalf("rotated token not usable: %+v %v", usable, err)
	}

	st.creds[7] = grokBuildServiceCreds("other-access", "other-refresh", "conn-2", "user-2")
	if _, err := svc.ResolveCreds(ctx, p, true); !IsCredentialConflict(err) || st.creds[7].RefreshToken != "other-refresh" {
		t.Fatalf("different account adopted: err=%v creds=%+v", err, st.creds[7])
	}
	same := grokBuildServiceCreds("reconnected-access", "reconnected-refresh", "conn-3", "user-1")
	st.creds[7] = same
	if _, err := svc.ResolveCreds(ctx, p, true); !IsCredentialConflict(err) || st.creds[7].GrokBuildConnection != "conn-3" {
		t.Fatalf("same-account reconnect was overwritten: err=%v creds=%+v", err, st.creds[7])
	}
	st.setConnection(7, domain.AuthOAuth, domain.ProtocolOpenAI)
	st.setBase(7, "https://api.x.ai/v1")
	if _, err := svc.ResolveCreds(ctx, p, false); !IsCredentialConflict(err) {
		t.Fatalf("wrong protocol refreshed: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("rejected paths exchanged a grant: hits=%d", hits.Load())
	}
}

func grokBuildServiceCreds(access, refresh, connection, user string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, Preset: grokbuild.Preset, GrokBuildAuth: true,
		AccessToken: access, RefreshToken: refresh, GrokBuildConnection: connection,
		UserID: user, Scopes: grokbuild.Scopes, AuthURL: grokbuild.Issuer,
		TokenURL: grokbuild.TokenURL, ClientID: grokbuild.ClientID,
	}
}

func cloneGrokBuild(c *domain.OAuthCreds) *domain.OAuthCreds {
	cp := *c
	return &cp
}

func TestMixedGrokBuildGrantDoesNotReachTokenEndpoint(t *testing.T) {
	var mu sync.Mutex
	var urls []string
	record := func(raw string) {
		mu.Lock()
		urls = append(urls, raw)
		mu.Unlock()
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), urls...)
	}
	reset := func() {
		mu.Lock()
		urls = nil
		mu.Unlock()
	}
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		record(r.URL.String())
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 60,
		}), nil
	}))
	restore := SetOpencodeConsoleClientForTest(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		record("console:" + r.URL.String())
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "console-access", "refresh_token": "console-refresh", "expires_in": 60,
		}), nil
	})})
	t.Cleanup(restore)

	mixed := grokBuildServiceCreds("access-secret", "refresh-secret", "conn-1", "user-1")
	mixed.OpencodeConsoleAuth = true
	mixed.OpencodeOrgID = "org-secret"
	mixed.ExpiresAt = 1
	st := newFakeStore()
	st.creds[9] = mixed
	st.setConnection(9, domain.AuthOAuth, domain.ProtocolGrokBuild)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	ctx := context.Background()
	p, err := st.GetProvider(ctx, 9)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"resolve", func() error { _, err := svc.ResolveCreds(ctx, p, false); return err }},
		{"force", func() error { _, err := svc.ResolveCreds(ctx, p, true); return err }},
		{"persist", func() error { _, err := svc.RefreshAndPersist(ctx, 9, cloneGrokBuild(mixed)); return err }},
		{"refresh", func() error { return refresh(ctx, cloneGrokBuild(mixed), time.Unix(10_000, 0)) }},
		{"direct", func() error { return refreshGrokBuild(ctx, cloneGrokBuild(mixed), time.Unix(10_000, 0)) }},
	} {
		err := call.fn()
		if err == nil {
			t.Fatalf("%s accepted mixed credentials", call.name)
		}
		for _, secret := range []string{"access-secret", "refresh-secret", "org-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s leaked %s: %v", call.name, secret, err)
			}
		}
	}
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("mixed grant reached a token endpoint: %v", got)
	}
	if st.writes.Load() != 0 || st.creds[9].AccessToken != "access-secret" || st.creds[9].RefreshToken != "refresh-secret" || st.creds[9].GrokBuildConnection != "conn-1" {
		t.Fatalf("mixed row changed writes=%d creds=%+v", st.writes.Load(), st.creds[9])
	}

	reset()
	console := &domain.OAuthCreds{
		OpencodeConsoleAuth: true, AccessToken: "console-old", RefreshToken: "console-refresh",
		OpencodeOrgID: "org-1", ExpiresAt: 1,
	}
	if err := refresh(ctx, console, time.Unix(10_000, 0)); err != nil {
		t.Fatal(err)
	}
	if console.AccessToken != "console-access" || len(snapshot()) != 1 || snapshot()[0] != "console:"+opencode.ConsoleTokenURL() {
		t.Fatalf("ordinary console refresh = %+v urls=%v", console, snapshot())
	}

	reset()
	valid := grokBuildServiceCreds("old-access", "old-refresh", "conn-1", "user-1")
	if err := refresh(ctx, valid, time.Unix(10_000, 0)); err != nil {
		t.Fatal(err)
	}
	if valid.AccessToken != "rotated-access" || len(snapshot()) != 1 || snapshot()[0] != grokbuild.TokenURL {
		t.Fatalf("ordinary grok refresh = %+v urls=%v", valid, snapshot())
	}
}

func TestMixedOpenCodeResolveCredsFailsClosed(t *testing.T) {
	var mu sync.Mutex
	var urls []string
	record := func(raw string) {
		mu.Lock()
		urls = append(urls, raw)
		mu.Unlock()
	}
	hits := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), urls...)
	}
	reset := func() {
		mu.Lock()
		urls = nil
		mu.Unlock()
	}
	SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		record(r.URL.String())
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 60,
		}), nil
	}))
	restore := SetOpencodeConsoleClientForTest(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		record("console:" + r.URL.String())
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "console-access", "refresh_token": "console-refresh", "expires_in": 60,
		}), nil
	})})
	t.Cleanup(restore)

	now := time.Unix(10_000, 0)
	unexpired := now.Add(time.Hour).Unix()
	ctx := context.Background()
	reject := func(t *testing.T, svc *Service, p *domain.Provider, secrets ...string) {
		t.Helper()
		for _, force := range []bool{false, true} {
			got, err := svc.ResolveCreds(ctx, p, force)
			if got != nil || !IsCredentialConflict(err) {
				t.Fatalf("force=%v creds=%v err=%v", force, got != nil, err)
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("force=%v leaked %s", force, secret)
				}
			}
		}
	}

	t.Run("unexpired mixed row", func(t *testing.T) {
		reset()
		mixed := grokBuildServiceCreds("access-secret", "refresh-secret", "conn-1", "user-1")
		mixed.OpencodeConsoleAuth = true
		mixed.OpencodeOrgID = "org-secret"
		mixed.ExpiresAt = unexpired
		st := newFakeStore()
		st.creds[11] = mixed
		st.setConnection(11, domain.AuthOAuth, domain.ProtocolOpencode)
		svc := New(st)
		svc.now = func() time.Time { return now }
		p, err := st.GetProvider(ctx, 11)
		if err != nil {
			t.Fatal(err)
		}
		reject(t, svc, p, "access-secret", "refresh-secret", "org-secret")
		if got := hits(); len(got) != 0 {
			t.Fatalf("mixed OpenCode row reached a token endpoint: %v", got)
		}
		if st.writes.Load() != 0 || st.creds[11].AccessToken != "access-secret" || st.creds[11].RefreshToken != "refresh-secret" {
			t.Fatalf("mixed OpenCode row changed writes=%d", st.writes.Load())
		}
	})

	// force reuses a newer live grant when the generation changed, without refresh.
	t.Run("ordinary snapshot mixed live", func(t *testing.T) {
		reset()
		ordinary := &domain.OAuthCreds{
			OpencodeConsoleAuth: true, AccessToken: "old-access", RefreshToken: "old-refresh",
			OpencodeOrgID: "org-1", ExpiresAt: unexpired,
		}
		live := cloneConsoleCreds(ordinary)
		live.GrokBuildAuth = true
		live.Preset = grokbuild.Preset
		live.AccessToken = "newer-secret"
		live.RefreshToken = "newer-refresh"
		live.ExpiresAt = unexpired + 60
		st := newFakeStore()
		st.creds[12] = live
		st.setConnection(12, domain.AuthOAuth, domain.ProtocolOpencode)
		svc := New(st)
		svc.now = func() time.Time { return now }
		p := &domain.Provider{
			ID: 12, BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
			AuthMethod: domain.AuthOAuth, OAuthCreds: ordinary,
		}
		reject(t, svc, p, "newer-secret", "newer-refresh", "old-access", "old-refresh")
		if got := hits(); len(got) != 0 {
			t.Fatalf("mixed live row reached a token endpoint: %v", got)
		}
		if st.writes.Load() != 0 || st.creds[12].AccessToken != "newer-secret" || st.creds[12].RefreshToken != "newer-refresh" || !st.creds[12].GrokBuildAuth {
			t.Fatalf("mixed live row changed writes=%d", st.writes.Load())
		}
	})
}

func TestGrokBuildManualCreds(t *testing.T) {
	access := grokOAuthJWT(map[string]any{"sub": "access-sub", "exp": 1900000000})
	id := grokOAuthJWT(map[string]any{"sub": "id-user", "email": "id@example.com", "exp": 1800000000})
	creds, err := GrokBuildManualCreds(access, "refresh-token-value", id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Mode != domain.OAuthManual || !creds.GrokBuildAuth || creds.Preset != grokbuild.Preset {
		t.Fatalf("markers = %+v", creds)
	}
	if creds.AccessToken != access || creds.RefreshToken != "refresh-token-value" {
		t.Fatalf("tokens = %+v", creds)
	}
	if creds.GrokBuildConnection == "" {
		t.Fatal("missing connection generation")
	}
	if creds.UserID != "id-user" || creds.Email != "id@example.com" || creds.IDToken != "" {
		t.Fatalf("identity = %+v", creds)
	}
	if creds.ExpiresAt != 1900000000 {
		t.Fatalf("expiry = %d", creds.ExpiresAt)
	}
	if creds.AuthURL != grokbuild.Issuer || creds.TokenURL != grokbuild.TokenURL || creds.ClientID != grokbuild.ClientID || creds.Scopes != grokbuild.Scopes {
		t.Fatalf("fixed metadata = %+v", creds)
	}
	second, err := GrokBuildManualCreds(access, "refresh-token-value", id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if second.GrokBuildConnection == creds.GrokBuildConnection {
		t.Fatal("manual imports reused a connection generation")
	}

	explicit, err := GrokBuildManualCreds(access, "refresh-token-value", "", 2000000000)
	if err != nil || explicit.ExpiresAt != 2000000000 {
		t.Fatalf("explicit expiry = %d err=%v", explicit.ExpiresAt, err)
	}
	team := grokOAuthJWT(map[string]any{"principal_type": "Team", "principal_id": "team-1"})
	teamCreds, err := GrokBuildManualCreds(team, "refresh-token-value", id, 0)
	if err != nil || teamCreds.UserID != "team-1" || teamCreds.Email != "" {
		t.Fatalf("team identity = %+v err=%v", teamCreds, err)
	}
	opaque, err := GrokBuildManualCreds("opaque-access-token", "refresh-token-value", "", 0)
	if err != nil || opaque.UserID != "" || opaque.Email != "" || opaque.ExpiresAt != 0 {
		t.Fatalf("opaque/no-id creds = %+v err=%v", opaque, err)
	}
}

func TestGrokBuildManualExpiryUsesAccessTokenOnly(t *testing.T) {
	idOnly := grokOAuthJWT(map[string]any{"sub": "id-user", "email": "id@example.com", "exp": 120})
	opaque, err := GrokBuildManualCreds("opaque-access-token", "refresh-token-value", idOnly, 0)
	if err != nil || opaque.ExpiresAt != 0 || opaque.UserID != "id-user" || opaque.Email != "id@example.com" {
		t.Fatalf("id-token expiry affected identity or access expiry: %+v err=%v", opaque, err)
	}

	fractionalID := grokOAuthJWT(map[string]any{"sub": "fractional-id", "exp": 120.5})
	fractional, err := GrokBuildManualCreds("opaque-access-token", "refresh-token-value", fractionalID, 0)
	if err != nil || fractional.ExpiresAt != 0 || fractional.UserID != "fractional-id" {
		t.Fatalf("fractional id expiry dropped identity: %+v err=%v", fractional, err)
	}

	fractionalTeam := grokOAuthJWT(map[string]any{"principal_type": "Team", "principal_id": "team-fractional", "exp": 120.5})
	team, err := GrokBuildManualCreds(fractionalTeam, "refresh-token-value", "", 0)
	if err != nil || team.ExpiresAt != 0 || team.UserID != "team-fractional" {
		t.Fatalf("fractional access expiry dropped team identity: %+v err=%v", team, err)
	}

	malformedTeam := grokOAuthJWT(map[string]any{"principal_type": "Organization", "principal_id": "org-malformed", "exp": "tomorrow"})
	malformed, err := GrokBuildManualCreds(malformedTeam, "refresh-token-value", fractionalID, 0)
	if err != nil || malformed.ExpiresAt != 0 || malformed.UserID != "org-malformed" {
		t.Fatalf("malformed exp dropped organization identity: %+v err=%v", malformed, err)
	}

	access := grokOAuthJWT(map[string]any{"sub": "access-user", "exp": 1900000000})
	explicit, err := GrokBuildManualCreds(access, "refresh-token-value", idOnly, 2000000000)
	if err != nil || explicit.ExpiresAt != 2000000000 {
		t.Fatalf("explicit expiry = %d err=%v", explicit.ExpiresAt, err)
	}
}

func TestGrokBuildManualCredsValidation(t *testing.T) {
	long := strings.Repeat("a", 64<<10+1)
	for _, tc := range []struct {
		name      string
		access    string
		refresh   string
		id        string
		expiresAt int64
		wantErr   string
	}{
		{name: "empty access", refresh: "refresh-token-value", wantErr: "access token is required"},
		{name: "empty refresh", access: "access-token-value", wantErr: "refresh token is required"},
		{name: "short access", access: "short", refresh: "refresh-token-value", wantErr: "access token is too short"},
		{name: "long refresh", access: "access-token-value", refresh: long, wantErr: "refresh token is too long"},
		{name: "control in refresh", access: "access-token-value", refresh: "refresh\nvalue", wantErr: "refresh token is malformed"},
		{name: "control in id token", access: "access-token-value", refresh: "refresh-token-value", id: "id\ntoken", wantErr: "id token is malformed"},
		{name: "public key", access: "xai-public-key-value", refresh: "refresh-token-value", wantErr: "public xAI API key"},
		{name: "negative expiry", access: "access-token-value", refresh: "refresh-token-value", expiresAt: -1, wantErr: "expiry is invalid"},
		{name: "large expiry", access: "access-token-value", refresh: "refresh-token-value", expiresAt: grokBuildExpiryMax + 1, wantErr: "expiry is invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GrokBuildManualCreds(tc.access, tc.refresh, tc.id, tc.expiresAt)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			for _, secret := range []string{tc.access, tc.refresh, tc.id} {
				if len(secret) >= 8 && strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked a token: %v", err)
				}
			}
		})
	}
}

func TestGrokBuildManualRefreshAccountGuard(t *testing.T) {
	personalID := func(sub string) string {
		return grokOAuthJWT(map[string]any{"sub": sub})
	}
	teamAccess := func(id string) string {
		return grokOAuthJWT(map[string]any{"principal_type": "Team", "principal_id": id})
	}
	for _, tc := range []struct {
		name            string
		mode            domain.OAuthMode
		currentUser     string
		currentAccess   string
		refreshedAccess string
		refreshedID     string
		wantErr         bool
		wantWrites      int64
		wantUser        string
	}{
		{name: "manual same person", mode: domain.OAuthManual, currentUser: "user-1", refreshedID: personalID("user-1"), wantWrites: 1, wantUser: "user-1"},
		{name: "manual different person", mode: domain.OAuthManual, currentUser: "user-1", refreshedID: personalID("user-2"), wantErr: true, wantWrites: 0, wantUser: "user-1"},
		{name: "manual personal to team", mode: domain.OAuthManual, currentUser: "old-user", refreshedAccess: teamAccess("other-team"), wantErr: true, wantWrites: 0, wantUser: "old-user"},
		{name: "manual team to personal", mode: domain.OAuthManual, currentUser: "old-team", currentAccess: teamAccess("old-team"), refreshedID: personalID("personal-user"), wantErr: true, wantWrites: 0, wantUser: "old-team"},
		{name: "manual same team", mode: domain.OAuthManual, currentUser: "team-1", currentAccess: teamAccess("team-1"), refreshedAccess: teamAccess("team-1"), wantWrites: 1, wantUser: "team-1"},
		{name: "device keeps person behavior", mode: domain.OAuthAuto, currentUser: "user-1", refreshedID: personalID("user-2"), wantWrites: 1, wantUser: "user-2"},
		{name: "device keeps source behavior", mode: domain.OAuthAuto, currentUser: "old-user", refreshedAccess: teamAccess("other-team"), wantWrites: 1, wantUser: "other-team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetGrokBuildTestTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != grokbuild.TokenURL {
					t.Fatalf("refresh left the fixed token host: %s", r.URL)
				}
				body, _ := io.ReadAll(r.Body)
				form, _ := url.ParseQuery(string(body))
				if form.Get("refresh_token") != "old-refresh" {
					t.Fatalf("refresh form = %v", form)
				}
				rotatedAccess := tc.refreshedAccess
				if rotatedAccess == "" {
					rotatedAccess = "rotated-access"
				}
				doc := map[string]any{
					"access_token": rotatedAccess, "refresh_token": "rotated-refresh", "expires_in": 600,
				}
				if tc.refreshedID != "" {
					doc["id_token"] = tc.refreshedID
				}
				return jsonResponse(http.StatusOK, doc), nil
			}))
			st := newFakeStore()
			basis := grokBuildServiceCreds("old-access", "old-refresh", "conn-1", tc.currentUser)
			if tc.currentAccess != "" {
				basis.AccessToken = tc.currentAccess
			}
			basis.Mode = tc.mode
			basis.ExpiresAt = 1
			st.creds[7] = basis
			st.setConnection(7, domain.AuthOAuth, domain.ProtocolGrokBuild)
			svc := New(st)
			svc.now = func() time.Time { return time.Unix(10_000, 0) }
			got, err := svc.RefreshAndPersist(context.Background(), 7, cloneGrokBuild(basis))
			if tc.wantErr {
				if !IsGrokBuildAccountMismatch(err) {
					t.Fatalf("err = %v, want account mismatch", err)
				}
				if got != nil {
					t.Fatalf("mismatch returned rotated credentials: %+v", got)
				}
				if st.writes.Load() != 0 {
					t.Fatalf("mismatch wrote %d times", st.writes.Load())
				}
				if st.creds[7].RefreshToken != "old-refresh" || st.creds[7].AccessToken != basis.AccessToken {
					t.Fatalf("mismatch changed row: %+v", st.creds[7])
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantAccess := tc.refreshedAccess
			if wantAccess == "" {
				wantAccess = "rotated-access"
			}
			if got == nil || got.AccessToken != wantAccess || got.RefreshToken != "rotated-refresh" {
				t.Fatalf("rotated = %+v", got)
			}
			if got.GrokBuildConnection != "conn-1" {
				t.Fatalf("routine refresh changed generation: %+v", got)
			}
			if st.writes.Load() != tc.wantWrites {
				t.Fatalf("writes = %d, want %d", st.writes.Load(), tc.wantWrites)
			}
			if st.creds[7].UserID != tc.wantUser || st.creds[7].Mode != tc.mode || st.creds[7].GrokBuildConnection != "conn-1" {
				t.Fatalf("persisted = %+v", st.creds[7])
			}
		})
	}
}

func grokOAuthJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
