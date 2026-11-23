package oauth

import (
	"context"
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
