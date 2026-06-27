package oauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

// fakeStore is a ProviderStore that records OAuth writes and lets tests inspect
// the last persisted creds. It also counts refresh-triggered writes.
type fakeStore struct {
	mu       sync.Mutex
	creds    map[int64]*domain.OAuthCreds
	writes   atomic.Int64
	writeErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{creds: map[int64]*domain.OAuthCreds{}}
}

func (f *fakeStore) GetProvider(_ context.Context, id int64) (*domain.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[id]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *c
	return &domain.Provider{ID: id, AuthMethod: domain.AuthOAuth, OAuthCreds: &cp}, nil
}

func (f *fakeStore) UpdateProviderOAuth(_ context.Context, id int64, creds *domain.OAuthCreds) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	cp := *creds
	f.creds[id] = &cp
	f.writes.Add(1)
	return nil
}

// tokenTestServer returns a token endpoint whose handler inspects the refresh
// request and responds with a configurable body + status. The handler records
// the form values seen.
func tokenTestServer(t *testing.T, fn func(form url.Values) (status int, body string)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = r.ParseForm()
		status, body := fn(r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newCreds(srv *httptest.Server) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode:         domain.OAuthManual,
		TokenURL:     srv.URL,
		ClientID:     "cid",
		RefreshToken: "rt-old",
		AccessToken:  "tok-old",
		ExpiresAt:    1, // expired
	}
}

func TestRefreshSuccess(t *testing.T) {
	srv, hits := tokenTestServer(t, func(form url.Values) (int, string) {
		if form.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", form.Get("grant_type"))
		}
		if form.Get("client_id") != "cid" {
			t.Errorf("client_id = %q", form.Get("client_id"))
		}
		if form.Get("refresh_token") != "rt-old" {
			t.Errorf("refresh_token = %q", form.Get("refresh_token"))
		}
		if form.Get("client_secret") != "" {
			t.Errorf("public client should not send client_secret")
		}
		return 200, `{"access_token":"tok-new","refresh_token":"rt-new","expires_in":3600,"id_token":"` + idToken(t, "u@x.com") + `"}`
	})
	c := newCreds(srv)
	now := time.Unix(1000, 0)

	if err := refresh(context.Background(), c, now); err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "tok-new" {
		t.Errorf("access = %q", c.AccessToken)
	}
	if c.RefreshToken != "rt-new" {
		t.Errorf("refresh = %q (want rotated)", c.RefreshToken)
	}
	if c.Email != "u@x.com" {
		t.Errorf("email = %q", c.Email)
	}
	if c.ExpiresAt != now.Add(3600*time.Second).Unix() {
		t.Errorf("expires_at = %d", c.ExpiresAt)
	}
	if hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1", hits.Load())
	}
}

func TestRefreshKeepsOldTokenWhenNotRotated(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok-new","expires_in":60}`
	})
	c := newCreds(srv)
	if err := refresh(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.RefreshToken != "rt-old" {
		t.Errorf("refresh = %q, want kept rt-old", c.RefreshToken)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 400, `{"error":"invalid_grant"}`
	})
	c := newCreds(srv)
	err := refresh(context.Background(), c, time.Now())
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	// Failed refresh must not mutate the caller's creds.
	if c.AccessToken != "tok-old" {
		t.Errorf("access changed on failure: %q", c.AccessToken)
	}
}

func TestShouldRefreshGate(t *testing.T) {
	now := time.Unix(10000, 0)
	cases := []struct {
		name  string
		creds *domain.OAuthCreds
		want  bool
	}{
		{"nil", nil, false},
		{"unknown expiry", &domain.OAuthCreds{ExpiresAt: 0}, false},
		{"far future", &domain.OAuthCreds{ExpiresAt: now.Add(1 * time.Hour).Unix()}, false},
		{"within lead", &domain.OAuthCreds{ExpiresAt: now.Add(2 * time.Minute).Unix()}, true},
		{"already expired", &domain.OAuthCreds{ExpiresAt: now.Add(-1 * time.Minute).Unix()}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRefresh(tc.creds, now); got != tc.want {
				t.Errorf("shouldRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveAPIKeyPassthrough(t *testing.T) {
	s := New(newFakeStore())
	tok, err := s.Resolve(context.Background(), &domain.Provider{
		AuthMethod: domain.AuthAPIKey, APIKey: "static-key",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "static-key" {
		t.Errorf("apikey resolve = %q", tok)
	}
}

func TestResolveProactiveRefreshPersists(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok-fresh","refresh_token":"rt-fresh","expires_in":3600}`
	})
	store := newFakeStore()
	// Seed the store with the provider's existing creds; Resolve re-reads from
	// the store (doRefresh works against the live creds).
	creds := newCreds(srv)
	creds.ExpiresAt = 1
	store.creds[7] = creds

	s := New(store)
	p := &domain.Provider{ID: 7, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}

	tok, err := s.Resolve(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-fresh" {
		t.Errorf("token = %q, want tok-fresh", tok)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
	// Persisted creds must reflect the rotation.
	store.mu.Lock()
	persisted := store.creds[7]
	store.mu.Unlock()
	if persisted.AccessToken != "tok-fresh" || persisted.RefreshToken != "rt-fresh" {
		t.Errorf("persisted = %+v", persisted)
	}
}

func TestResolveSkipsRefreshWhenNotExpiring(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"should-not-happen"}`
	})
	store := newFakeStore()
	s := New(store)
	creds := newCreds(srv)
	creds.ExpiresAt = s.now().Add(1 * time.Hour).Unix() // far future
	store.creds[1] = creds

	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-old" {
		t.Errorf("token = %q, want existing tok-old", tok)
	}
	if hits.Load() != 0 {
		t.Errorf("token endpoint should not be hit, got %d", hits.Load())
	}
}

func TestResolveForcedRefreshOn401(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok-forced","refresh_token":"rt-2","expires_in":3600}`
	})
	store := newFakeStore()
	s := New(store)
	creds := newCreds(srv)
	creds.ExpiresAt = s.now().Add(1 * time.Hour).Unix() // not near expiry
	store.creds[1] = creds

	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, true) // force (reactive 401)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-forced" {
		t.Errorf("token = %q, want tok-forced", tok)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}

// TestResolveDedupesConcurrentRefreshes fires N concurrent proactive resolves
// for an expired token; the token endpoint must be hit exactly once.
func TestResolveDedupesConcurrentRefreshes(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		time.Sleep(20 * time.Millisecond) // widen the window so callers overlap
		return 200, `{"access_token":"tok-x","refresh_token":"rt-x","expires_in":3600}`
	})
	store := newFakeStore()
	creds := newCreds(srv)
	creds.ExpiresAt = 1
	store.creds[1] = creds

	s := New(store)
	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Resolve(context.Background(), p, false); err != nil {
				t.Errorf("resolve err: %v", err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1 (dedup)", hits.Load())
	}
}

func TestResolveForcedFailureReturnsOldToken(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 400, `{"error":"server_error","error_description":"boom"}`
	})
	store := newFakeStore()
	creds := newCreds(srv)
	creds.AccessToken = "tok-stale"
	store.creds[1] = creds

	s := New(store)
	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, true)
	if err == nil {
		t.Fatal("want error from forced refresh failure")
	}
	if tok != "tok-stale" {
		t.Errorf("token = %q, want fallback tok-stale", tok)
	}
}

// idToken builds an unsigned JWT with the given email claim for tests.
func idToken(t *testing.T, email string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":%q}`, email)))
	return header + "." + payload + "."
}
