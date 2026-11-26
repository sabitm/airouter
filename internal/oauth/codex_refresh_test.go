package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/codexgrant"
	"airouter/internal/crypto"
	"airouter/internal/domain"
	"airouter/internal/store"
)

func codexServiceCreds(access, refresh, generation, account string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, Preset: codexgrant.Preset, ClientID: codexgrant.ClientID,
		TokenURL: codexgrant.TokenURL, RefreshJSON: true,
		AccessToken: access, RefreshToken: refresh, AccountID: account,
		CodexGrantGeneration: generation,
	}
}

func cloneCodex(c *domain.OAuthCreds) *domain.OAuthCreds {
	cp := *c
	return &cp
}

func fakeCodexIDToken(account string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{
		"email":                       "u@codex.test",
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account},
		"account_id":                  account,
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func TestCodexRefreshKeepsGenerationAndInitializesLegacy(t *testing.T) {
	var seen atomic.Value
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		seen.Store(append([]byte(nil), body...))
		if r.URL.String() != codexgrant.TokenURL {
			t.Errorf("token URL = %s", r.URL.String())
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 600,
			"id_token": fakeCodexIDToken("acct-1"),
		}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	legacy := codexServiceCreds("old-access", "old-refresh", "", "acct-1")
	legacy.ExpiresAt = 1
	st.creds[4] = cloneCodex(legacy)
	st.setConnection(4, domain.AuthOAuth, domain.ProtocolOpenAICodex)
	st.setBase(4, codexgrant.APIBase)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	p, err := st.GetProvider(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.ResolveCreds(context.Background(), p, false)
	if err != nil || got == nil || got.AccessToken != "rotated-access" || got.CodexGrantGeneration == "" {
		t.Fatalf("legacy refresh = %+v %v", got, err)
	}
	var req map[string]string
	raw, _ := seen.Load().([]byte)
	_ = json.Unmarshal(raw, &req)
	if req["grant_type"] != "refresh_token" || req["client_id"] != codexgrant.ClientID || req["refresh_token"] != "old-refresh" {
		t.Fatalf("refresh body = %s", raw)
	}
	if st.creds[4].RefreshToken != "rotated-refresh" || st.creds[4].CodexGrantGeneration != got.CodexGrantGeneration || st.creds[4].AccountID != "acct-1" || st.creds[4].TokenURL != codexgrant.TokenURL {
		t.Fatalf("persisted = %+v", st.creds[4])
	}
	stable := got.CodexGrantGeneration
	st.creds[4].ExpiresAt = 1
	again, err := svc.ResolveCreds(context.Background(), &domain.Provider{
		ID: 4, BaseURL: codexgrant.APIBase, Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, OAuthCreds: cloneCodex(st.creds[4]),
	}, true)
	if err != nil || again.CodexGrantGeneration != stable || again.AccessToken != "rotated-access" {
		t.Fatalf("second refresh = %+v %v", again, err)
	}
}

func TestCodexRefreshDoesNotOverwriteReconnect(t *testing.T) {
	entered := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	var hits atomic.Int32
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		close(entered)
		<-hold
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 600,
			"id_token": fakeCodexIDToken("acct-1"),
		}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	basis := codexServiceCreds("old-access", "old-refresh", "gen-1", "acct-1")
	basis.ExpiresAt = 1
	st.creds[8] = cloneCodex(basis)
	st.setConnection(8, domain.AuthOAuth, domain.ProtocolOpenAICodex)
	st.setBase(8, codexgrant.APIBase)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	p, err := st.GetProvider(context.Background(), 8)
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
		t.Fatal("refresh did not reach token endpoint")
	}
	replaced := codexServiceCreds("reconnected-access", "reconnected-refresh", "gen-2", "acct-1")
	st.creds[8] = replaced
	release()
	select {
	case err := <-errCh:
		if !IsCredentialConflict(err) {
			t.Fatalf("reconnect during refresh = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("refresh did not finish")
	}
	if st.creds[8].AccessToken != "reconnected-access" || st.creds[8].CodexGrantGeneration != "gen-2" || st.writes.Load() != 0 {
		t.Fatalf("row overwritten: %+v writes=%d hits=%d", st.creds[8], st.writes.Load(), hits.Load())
	}
}

func TestCodexRefreshRotatesGenerationWhenAccountChanges(t *testing.T) {
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "other-access", "refresh_token": "other-refresh", "expires_in": 600,
			"id_token": fakeCodexIDToken("acct-2"),
		}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	basis := codexServiceCreds("old-access", "old-refresh", "gen-1", "acct-1")
	basis.ExpiresAt = 1
	st.creds[9] = cloneCodex(basis)
	st.setConnection(9, domain.AuthOAuth, domain.ProtocolOpenAICodex)
	st.setBase(9, codexgrant.APIBase)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	p, err := st.GetProvider(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.ResolveCreds(context.Background(), p, true)
	if err != nil || got.AccountID != "acct-2" || got.CodexGrantGeneration == "" || got.CodexGrantGeneration == "gen-1" {
		t.Fatalf("account rotation = %+v %v", got, err)
	}
}

func TestCodexRefreshDoesNotHandleOrdinaryOAuth(t *testing.T) {
	var hits atomic.Int32
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{"access_token": "ordinary", "refresh_token": "ordinary", "expires_in": 600}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	creds := &domain.OAuthCreds{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1, TokenURL: "https://example.test/token"}
	st.creds[11] = creds
	st.setConnection(11, domain.AuthOAuth, domain.ProtocolOpenAI)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }
	p := &domain.Provider{ID: 11, Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, OAuthCreds: cloneCodex(creds)}
	got, err := svc.ResolveCreds(context.Background(), p, false)
	if err != nil || got.AccessToken != "ordinary" || hits.Load() != 1 || st.creds[11].CodexGrantGeneration != "" {
		t.Fatalf("ordinary = %+v err=%v hits=%d", got, err, hits.Load())
	}
}

func TestCodexSavedRefreshIgnoresReconfiguredTokenURL(t *testing.T) {
	var urls []string
	entered := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		urls = append(urls, r.URL.String())
		if r.URL.String() != codexgrant.TokenURL {
			t.Errorf("token URL = %s", r.URL.String())
		}
		close(entered)
		<-hold
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 600,
			"id_token": fakeCodexIDToken("acct"),
		}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	stored := codexServiceCreds("old-access", "old-refresh", "gen-old", "acct")
	st.creds[8] = cloneCodex(stored)
	st.setConnection(8, domain.AuthOAuth, domain.ProtocolOpenAICodex)
	st.setBase(8, codexgrant.APIBase)
	svc := New(st)
	svc.now = func() time.Time { return time.Unix(10_000, 0) }

	altered := cloneCodex(stored)
	altered.TokenURL = "https://mock.example.test/token"
	altered.CodexGrantGeneration = ""
	altered.ClientID = "other-client"
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := svc.RefreshAndPersist(ctx, 8, altered)
		done <- err
	}()
	select {
	case err := <-done:
		if !IsCredentialConflict(err) {
			t.Fatalf("altered refresh = %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if len(urls) != 0 || st.writes.Load() != 0 {
		t.Fatalf("altered input escaped: urls=%v writes=%d", urls, st.writes.Load())
	}

	compatible := cloneCodex(stored)
	compatible.CodexGrantGeneration = ""
	compatible.RefreshToken = "form-refresh"
	go func() {
		_, err := svc.RefreshAndPersist(ctx, 8, compatible)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("compatible refresh returned early: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	st.creds[8] = codexServiceCreds("new-access", "new-refresh", "gen-new", "acct")
	release()
	select {
	case err := <-done:
		if !IsCredentialConflict(err) || st.creds[8].CodexGrantGeneration != "gen-new" || st.creds[8].AccessToken != "new-access" || st.writes.Load() != 0 {
			t.Fatalf("reconnect overwritten: err=%v creds=%+v writes=%d", err, st.creds[8], st.writes.Load())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, u := range urls {
		if u != codexgrant.TokenURL {
			t.Fatalf("unexpected token URL %s", u)
		}
	}
}

func TestCustomRefreshCannotOverwriteCodexReconnect(t *testing.T) {
	for _, proto := range []domain.Protocol{domain.ProtocolOpenAICodex, domain.ProtocolOpenAI} {
		t.Run(string(proto), func(t *testing.T) {
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			srv, hits := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
				return http.StatusOK, `{"access_token":"old-rotation","refresh_token":"old-refresh-rotated","expires_in":600}`
			})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			cipher, err := crypto.New("codex-refresh-test")
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "refresh.db"), cipher)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()
			custom := newCreds(srv)
			custom.AccountID = "acct"
			p := &domain.Provider{Name: "custom", BaseURL: srv.URL, Protocol: proto, AuthMethod: domain.AuthOAuth, OAuthCreds: custom}
			if err := st.CreateProvider(ctx, p); err != nil {
				t.Fatal(err)
			}
			svc := New(st)
			done := make(chan error, 1)
			go func(id int64) {
				_, err := svc.RefreshAndPersist(ctx, id, custom)
				done <- err
			}(p.ID)
			waitN(t, entered, 1)
			p.BaseURL, p.Protocol = codexgrant.APIBase, domain.ProtocolOpenAICodex
			p.OAuthCreds = codexServiceCreds("new-access", "new-refresh", "gen-new", "acct")
			if err := st.UpdateProvider(ctx, p); err != nil {
				t.Fatal(err)
			}
			unblock()
			select {
			case err := <-done:
				if !IsCredentialConflict(err) {
					t.Fatalf("obsolete refresh returned %v, want credential conflict", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			live, err := st.GetProvider(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !codexRefreshable(live) || live.OAuthCreds.CodexGrantGeneration != "gen-new" ||
				live.OAuthCreds.AccessToken != "new-access" || live.OAuthCreds.RefreshToken != "new-refresh" || hits.Load() != 1 {
				t.Fatal("obsolete refresh changed the new Codex connection")
			}
		})
	}
}

func TestCodexRefreshRejectsStaleAuthRetry(t *testing.T) {
	var hits atomic.Int32
	orig := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{"access_token": "should-not", "refresh_token": "should-not"}), nil
	})}
	t.Cleanup(func() { httpClient = orig })
	st := newFakeStore()
	live := codexServiceCreds("fresh-access", "fresh-refresh", "gen-1", "acct-1")
	live.ExpiresAt = time.Now().Add(time.Hour).Unix()
	st.creds[10] = cloneCodex(live)
	st.setConnection(10, domain.AuthOAuth, domain.ProtocolOpenAICodex)
	st.setBase(10, codexgrant.APIBase)
	svc := New(st)
	stale := &domain.Provider{
		ID: 10, BaseURL: codexgrant.APIBase, Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, OAuthCreds: codexServiceCreds("old-access", "old-refresh", "gen-1", "acct-1"),
	}
	got, err := svc.ResolveCreds(context.Background(), stale, true)
	if err != nil || got.AccessToken != "fresh-access" || hits.Load() != 0 {
		t.Fatalf("stale retry = %+v err=%v hits=%d", got, err, hits.Load())
	}
	other := &domain.Provider{
		ID: 10, BaseURL: codexgrant.APIBase, Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, OAuthCreds: codexServiceCreds("old-access", "old-refresh", "gen-old", "acct-1"),
	}
	if _, err := svc.ResolveCreds(context.Background(), other, true); !IsCredentialConflict(err) || hits.Load() != 0 {
		t.Fatalf("wrong generation = %v hits=%d", err, hits.Load())
	}
}
