package oauth

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"airouter/internal/domain"
)

func TestResolveAPIKeyIgnoresStaleOAuthSnapshot(t *testing.T) {
	svc := New(newFakeStore())
	p := &domain.Provider{
		AuthMethod: domain.AuthAPIKey,
		AuthScheme: domain.AuthXAPIKey,
		Protocol:   domain.ProtocolClaudeCode,
		APIKey:     "static",
		OAuthCreds: &domain.OAuthCreds{AccessToken: "stale", ClaudeCodeAuth: true},
	}
	tok, err := svc.Resolve(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "static" {
		t.Fatalf("apikey Resolve returned %q, want static", tok)
	}
	if p.APIKey != "static" {
		t.Fatalf("Resolve mutated APIKey to %q", p.APIKey)
	}
	creds, err := svc.ResolveCreds(context.Background(), p, true)
	if err != nil {
		t.Fatal(err)
	}
	if creds == nil || creds.AccessToken != "stale" {
		t.Fatalf("config snapshot = %+v", creds)
	}
	if p.OAuthCreds.AccessToken != "stale" {
		t.Fatal("ResolveCreds mutated the provider snapshot")
	}
}

// barrierCASStore holds both first-use initializers before either enters the
// compare-and-swap. The test does not depend on how many times the service
// reads the row; the barrier is on the write path itself.
type barrierCASStore struct {
	*fakeStore
	mu      sync.Mutex
	entered int
	ready   chan struct{}
	release chan struct{}
}

func (s *barrierCASStore) UpdateProviderOAuthCAS(ctx context.Context, id int64, update func(*domain.OAuthCreds) (*domain.OAuthCreds, bool, error)) (*domain.OAuthCreds, error) {
	s.mu.Lock()
	s.entered++
	if s.entered == 2 {
		close(s.ready)
	}
	s.mu.Unlock()
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.fakeStore.UpdateProviderOAuthCAS(ctx, id, update)
}

func TestConcurrentIdentityInitializesOneDevice(t *testing.T) {
	c := domain.OAuthCreds{
		ClaudeCodeAuth: true,
		AccessToken:    "synthetic",
		ExpiresAt:      time.Now().Add(time.Hour).Unix(),
	}
	st := &barrierCASStore{
		fakeStore: newFakeStore(),
		ready:     make(chan struct{}),
		release:   make(chan struct{}),
	}
	cp := c
	st.creds[1] = &cp
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		device string
		token  string
		acct   string
		err    error
	}
	ids := make(chan result, 2)
	// Two independent services share one store, matching two proxy instances.
	for i := 0; i < 2; i++ {
		svc := New(st)
		go func() {
			p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: &c}
			got, err := svc.ResolveCreds(ctx, p, false)
			if err != nil || got == nil {
				ids <- result{err: err}
				return
			}
			ids <- result{device: got.ClaudeDeviceID, token: got.AccessToken, acct: got.ClaudeAccountUUID}
		}()
	}
	close(st.release)
	a, b := <-ids, <-ids
	if a.err != nil || b.err != nil {
		t.Fatalf("resolve errors: %v / %v", a.err, b.err)
	}
	if !isDeviceID(a.device) || a.device != b.device {
		t.Fatalf("concurrent initialization produced device IDs %s / %s", a.device, b.device)
	}
	if a.token != "synthetic" || b.token != "synthetic" {
		t.Fatalf("token/account snapshot split: %q / %q", a.token, b.token)
	}
	st.mu.Lock()
	stored := st.creds[1].ClaudeDeviceID
	st.mu.Unlock()
	if stored != a.device {
		t.Fatalf("stored device %s != resolved %s", stored, a.device)
	}
}

// retryCASStore invokes the callback once on the old snapshot, publishes a
// reconnect, then delegates to the fake store. The delegated CAS must run the
// callback again against the newer row. The test therefore exercises a retry
// after the callback, not a pre-read barrier.
type retryCASStore struct {
	*fakeStore
	once      sync.Once
	reconnect *domain.OAuthCreds
}

func (s *retryCASStore) UpdateProviderOAuthCAS(ctx context.Context, id int64, update func(*domain.OAuthCreds) (*domain.OAuthCreds, bool, error)) (*domain.OAuthCreds, error) {
	var err error
	s.once.Do(func() {
		s.mu.Lock()
		cur := s.creds[id]
		var old *domain.OAuthCreds
		if cur != nil {
			cp := *cur
			old = &cp
		}
		s.mu.Unlock()
		if old != nil {
			_, _, err = update(old)
		}
		s.mu.Lock()
		if err == nil {
			cp := *s.reconnect
			s.creds[id] = &cp
		}
		s.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return s.fakeStore.UpdateProviderOAuthCAS(ctx, id, update)
}

func TestRefreshCASKeepsReconnect(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600,"scope":"user:inference"}`
	})
	store := &retryCASStore{fakeStore: newFakeStore()}
	old := &domain.OAuthCreds{
		ClaudeCodeAuth:    true,
		AccessToken:       "old-access",
		RefreshToken:      "old-refresh",
		ExpiresAt:         1,
		TokenURL:          srv.URL,
		ClientID:          "cid",
		ClaudeAccountUUID: "old-account",
		ClaudeDeviceID:    repeatHex("ab"),
		ClaudeScopes:      "user:inference",
	}
	store.creds[9] = cloneCreds(old)
	store.reconnect = &domain.OAuthCreds{
		ClaudeCodeAuth:    true,
		AccessToken:       "new-access",
		RefreshToken:      "new-refresh",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
		TokenURL:          srv.URL,
		ClientID:          "cid",
		ClaudeAccountUUID: "new-account",
		ClaudeDeviceID:    repeatHex("cd"),
		ClaudeScopes:      "user:profile",
	}
	svc := New(store)
	p := &domain.Provider{ID: 9, AuthMethod: domain.AuthOAuth, OAuthCreds: cloneCreds(old)}
	got, err := svc.ResolveCreds(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	stored := store.creds[9]
	if stored.ClaudeAccountUUID != "new-account" || stored.ClaudeDeviceID != repeatHex("cd") || stored.AccessToken != "new-access" {
		t.Fatalf("stale refresh overwrote newer row: %+v", stored)
	}
	if got == nil || got.AccessToken != "new-access" || got.ClaudeAccountUUID != "new-account" || got.ClaudeDeviceID != repeatHex("cd") {
		t.Fatalf("resolved snapshot did not pair the new token, account, and device: %+v", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", hits.Load())
	}
}

func TestRefreshCASMergesRotatedTokenIntoNewerDevice(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600,"scope":"user:inference"}`
	})
	st := newFakeStore()
	old := &domain.OAuthCreds{
		ClaudeCodeAuth:    true,
		AccessToken:       "old-access",
		RefreshToken:      "old-refresh",
		ExpiresAt:         1,
		TokenURL:          srv.URL,
		ClientID:          "cid",
		ClaudeAccountUUID: "account",
		ClaudeScopes:      "user:inference",
	}
	st.creds[4] = cloneCreds(old)
	// Device initialization committed after the refresh basis was selected.
	newer := cloneCreds(old)
	newer.ClaudeDeviceID = repeatHex("ef")
	st.creds[4] = newer
	svc := New(st)
	got, err := svc.ResolveCreds(context.Background(), &domain.Provider{ID: 4, AuthMethod: domain.AuthOAuth, OAuthCreds: cloneCreds(old)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" || got.ClaudeDeviceID != repeatHex("ef") {
		t.Fatalf("rotated token lost newer device: %+v", got)
	}
	stored := st.creds[4]
	if stored.AccessToken != "rotated-access" || stored.ClaudeDeviceID != repeatHex("ef") {
		t.Fatalf("stored merge = %+v", stored)
	}
}

func TestDeviceInitDeletedRowFails(t *testing.T) {
	svc := New(newFakeStore())
	creds := &domain.OAuthCreds{ClaudeCodeAuth: true, AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	got, err := svc.ResolveCreds(context.Background(), &domain.Provider{ID: 77, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}, false)
	if err == nil {
		t.Fatal("deleted row initialization was ignored")
	}
	if got != nil && isDeviceID(got.ClaudeDeviceID) {
		t.Fatalf("invented device for deleted row: %+v", got)
	}
}

func repeatHex(pair string) string {
	out := ""
	for len(out) < 64 {
		out += pair
	}
	return out[:64]
}

func cloneCreds(c *domain.OAuthCreds) *domain.OAuthCreds {
	cp := *c
	return &cp
}

func TestDeviceRandomFailureIsExplicit(t *testing.T) {
	old := randRead
	randRead = func(b []byte) (int, error) { return 0, errors.New("entropy unavailable") }
	t.Cleanup(func() { randRead = old })
	store := newFakeStore()
	creds := &domain.OAuthCreds{ClaudeCodeAuth: true, AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	store.creds[3] = cloneCreds(creds)
	svc := New(store)
	got, err := svc.ResolveCreds(context.Background(), &domain.Provider{ID: 3, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}, false)
	if err == nil {
		t.Fatal("random failure was ignored")
	}
	if got != nil && isDeviceID(got.ClaudeDeviceID) {
		t.Fatalf("invented device %s after random failure", got.ClaudeDeviceID)
	}
	if isDeviceID(store.creds[3].ClaudeDeviceID) {
		t.Fatal("store received an invented device")
	}
}
