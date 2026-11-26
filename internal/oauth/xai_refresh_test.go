package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/xaigrant"
)

func xaiJWT(sub string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": sub, "email": "not-proof@example.com"})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func xaiServiceCreds(access, refresh, generation, sub string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, Preset: xaigrant.Preset, ClientID: xaigrant.ClientID,
		TokenURL: xaigrant.TokenURL, AccessToken: access, RefreshToken: refresh,
		XAIGrantGeneration: generation, ExpiresAt: time.Now().Add(time.Hour).Unix(),
		// Keep a nonempty principal stable across a refresh even when the test
		// token endpoint rotates the access token.
		AccountID: sub,
	}
}

func TestXAIResolveRefreshPreservesGeneration(t *testing.T) {
	var hits atomic.Int32
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		if r.URL.String() != xaigrant.TokenURL {
			t.Errorf("token URL = %s", r.URL.String())
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 600,
			"id_token": xaiJWT("user-1"),
		}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	basis := xaiServiceCreds("old-access", "old-refresh", "gen-1", "user-1")
	basis.ExpiresAt = 1
	st.creds[4] = basis
	st.setConnection(4, domain.AuthOAuth, domain.ProtocolOpenAIResponses)
	st.setBase(4, xaigrant.APIBase)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	p, err := st.GetProvider(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.ResolveCreds(context.Background(), p, false)
	if err != nil || got == nil || got.AccessToken != "rotated-access" || got.XAIGrantGeneration != "gen-1" {
		t.Fatalf("refresh = %+v %v", got, err)
	}
	if st.creds[4].RefreshToken != "rotated-refresh" || st.creds[4].TokenURL != xaigrant.TokenURL || st.creds[4].ClientID != xaigrant.ClientID {
		t.Fatalf("persisted = %+v", st.creds[4])
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestXAIEnsureGenerationIsStableAndAtomic(t *testing.T) {
	st := newFakeStore()
	c := xaiServiceCreds("access", "refresh", "", "user-1")
	c.ExpiresAt = time.Now().Add(time.Hour).Unix()
	st.creds[5] = c
	st.setConnection(5, domain.AuthOAuth, domain.ProtocolOpenAIResponses)
	st.setBase(5, xaigrant.APIBase)
	svc := New(st)
	p, err := st.GetProvider(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.EnsureXAIGeneration(context.Background(), p)
	if err != nil || first == nil || first.XAIGrantGeneration == "" {
		t.Fatalf("init = %+v %v", first, err)
	}
	again, err := svc.EnsureXAIGeneration(context.Background(), p)
	if err != nil || again.XAIGrantGeneration != first.XAIGrantGeneration {
		t.Fatalf("second init = %+v %v", again, err)
	}
	if st.writes.Load() != 1 {
		t.Fatalf("writes = %d", st.writes.Load())
	}
}

func TestXAIReconnectDuringRefreshWins(t *testing.T) {
	entered := make(chan struct{})
	hold := make(chan struct{})
	var once atomic.Bool
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !once.Swap(true) {
			close(entered)
		}
		<-hold
		return jsonResponse(http.StatusOK, map[string]any{"access_token": "old-rotation", "refresh_token": "old-refresh-rotated", "expires_in": 600}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	basis := xaiServiceCreds("old-access", "old-refresh", "gen-1", "user-1")
	basis.ExpiresAt = 1
	st.creds[6] = basis
	st.setConnection(6, domain.AuthOAuth, domain.ProtocolOpenAIResponses)
	st.setBase(6, xaigrant.APIBase)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	p, err := st.GetProvider(context.Background(), 6)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := svc.ResolveCreds(ctx, p, true)
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("refresh did not reach token endpoint")
	}
	st.creds[6] = xaiServiceCreds("new-access", "new-refresh", "gen-2", "user-1")
	st.creds[6].ExpiresAt = time.Now().Add(time.Hour).Unix()
	close(hold)
	if err := <-done; !IsCredentialConflict(err) {
		t.Fatalf("reconnect race = %v", err)
	}
	if st.creds[6].AccessToken != "new-access" || st.creds[6].XAIGrantGeneration != "gen-2" {
		t.Fatalf("reconnect overwritten = %+v", st.creds[6])
	}
}

func TestXAISavedRefreshRejectsOverridesAndFollowsStoredGrant(t *testing.T) {
	var hits atomic.Int32
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 600,
		}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	stored := xaiServiceCreds("old-access", "old-refresh", "gen-1", "user-1")
	stored.ExpiresAt = 1
	st.creds[7] = stored
	st.setConnection(7, domain.AuthOAuth, domain.ProtocolOpenAI)
	st.setBase(7, xaigrant.APIBase)
	svc := New(st)

	altered := xaiServiceCreds("old-access", "old-refresh", "", "user-1")
	altered.TokenURL = "https://mock.example.test/token"
	altered.ClientID = "other-client"
	altered.Preset = "custom"
	if _, err := svc.RefreshAndPersist(context.Background(), 7, altered); !IsCredentialConflict(err) {
		t.Fatalf("altered refresh = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("altered input reached token endpoint")
	}

	compatible := xaiServiceCreds("form-access", "form-refresh", "", "user-1")
	got, err := svc.RefreshAndPersist(context.Background(), 7, compatible)
	if err != nil || got == nil || got.AccessToken != "rotated-access" || got.XAIGrantGeneration != "gen-1" {
		t.Fatalf("stored refresh = %+v %v", got, err)
	}
	if hits.Load() != 1 || st.creds[7].AccessToken != "rotated-access" || st.creds[7].XAIGrantGeneration != "gen-1" {
		t.Fatalf("hits=%d row=%+v", hits.Load(), st.creds[7])
	}
}

func TestXAIRefreshRejectsForeignStoredGrant(t *testing.T) {
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, map[string]any{"access_token": "should-not", "refresh_token": "should-not"}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	stored := xaiServiceCreds("access", "refresh", "gen", "user")
	st.creds[8] = stored
	st.setConnection(8, domain.AuthOAuth, domain.ProtocolOpenAIResponses)
	st.setBase(8, xaigrant.APIBase)
	svc := New(st)
	foreign := xaiServiceCreds("access", "refresh", "gen", "user")
	foreign.ClientID = "foreign"
	if _, err := svc.RefreshAndPersist(context.Background(), 8, foreign); !IsCredentialConflict(err) {
		t.Fatalf("foreign refresh = %v", err)
	}
}

func TestXAIResolveDoesNotHandleOrdinaryOAuth(t *testing.T) {
	var hits atomic.Int32
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{"access_token": "ordinary", "refresh_token": "ordinary", "expires_in": 600}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	creds := &domain.OAuthCreds{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1, TokenURL: "https://example.test/token", ClientID: "cid"}
	st.creds[9] = creds
	st.setConnection(9, domain.AuthOAuth, domain.ProtocolOpenAI)
	svc := New(st)
	p := &domain.Provider{ID: 9, Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	got, err := svc.ResolveCreds(context.Background(), p, false)
	if err != nil || got.AccessToken != "ordinary" || hits.Load() != 1 || st.creds[9].XAIGrantGeneration != "" {
		t.Fatalf("ordinary = %+v err=%v hits=%d", got, err, hits.Load())
	}
}
