package oauth

import (
	"context"
	"errors"
	"net/url"
	"strings"
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

func TestSavedClaudeRefreshRejectsReconnectDuringCall(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv, hits := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 200, `{"access_token":"old-result","expires_in":3600}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[1] = cloneCreds(stored)
	svc := New(st)
	req := ClaudeRefreshRequest{Stored: cloneCreds(stored), Effective: cloneCreds(stored), Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode}

	type result struct {
		creds *domain.OAuthCreds
		err   error
	}
	done := make(chan result, 1)
	go func() {
		got, err := svc.RefreshSavedClaude(context.Background(), 1, req)
		done <- result{got, err}
	}()
	waitN(t, entered, 1)

	reconnect := cloneCreds(stored)
	reconnect.AccessToken = "reconnect-access"
	reconnect.RefreshToken = "reconnect-refresh"
	reconnect.ClaudeAccountUUID = "reconnect-account"
	reconnect.AccountID = "reconnect-account"
	reconnect.ClaudeDeviceID = repeatHex("cd")
	st.mu.Lock()
	st.creds[1] = reconnect
	st.mu.Unlock()
	close(release)

	got := <-done
	if !IsCredentialConflict(got.err) {
		t.Fatalf("err = %v, want credential conflict", got.err)
	}
	if got.creds != nil {
		t.Fatalf("conflict returned discarded credentials: %+v", got.creds)
	}
	st.mu.Lock()
	final := st.creds[1]
	st.mu.Unlock()
	if final.AccessToken != "reconnect-access" || final.RefreshToken != "reconnect-refresh" || final.ClaudeAccountUUID != "reconnect-account" {
		t.Fatalf("old refresh overwrote reconnect: %+v", final)
	}
	if hits.Load() != 1 {
		t.Fatalf("token hits = %d", hits.Load())
	}
}

func TestSavedClaudeRefreshDetectsSameTokenRotation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv, _ := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 200, `{"access_token":"obsolete-access","expires_in":3600}`
	})
	winnerSrv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"winner-access","expires_in":7200}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[2] = cloneCreds(stored)
	first := New(st)
	second := New(st)
	req := ClaudeRefreshRequest{Stored: cloneCreds(stored), Effective: cloneCreds(stored), Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode}
	winnerBasis := cloneCreds(stored)
	winnerBasis.TokenURL = winnerSrv.URL

	type result struct {
		creds *domain.OAuthCreds
		err   error
	}
	done := make(chan result, 1)
	go func() {
		got, err := first.RefreshSavedClaude(context.Background(), 2, req)
		done <- result{got, err}
	}()
	waitN(t, entered, 1)
	winner, err := second.RefreshSavedClaude(context.Background(), 2, ClaudeRefreshRequest{
		Stored: cloneCreds(stored), Effective: winnerBasis,
		Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if winner.AccessToken != "winner-access" {
		t.Fatalf("winner did not rotate: %+v", winner)
	}
	close(release)
	got := <-done
	if !IsCredentialConflict(got.err) || got.creds != nil {
		t.Fatalf("same-token rotation was overwritten: creds %+v err %v", got.creds, got.err)
	}
	st.mu.Lock()
	final := st.creds[2]
	st.mu.Unlock()
	if final.AccessToken != winner.AccessToken || final.ExpiresAt != winner.ExpiresAt || final.RefreshToken != stored.RefreshToken {
		t.Fatalf("stored row = %+v, winner %+v", final, winner)
	}
}

func TestSavedClaudeReplacementConflictsAndClearsUnknownGrant(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	success, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"replacement-access","expires_in":3600}`
	})
	conflictSrv, _ := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 200, `{"access_token":"obsolete-replacement","expires_in":3600}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(success.URL)
	st.creds[3] = cloneCreds(stored)
	svc := New(st)
	effective := cloneCreds(stored)
	effective.RefreshToken = "replacement-refresh"
	effective.AccessToken = ""
	effective.ExpiresAt = 0
	effective.Email = ""
	effective.AccountID = ""
	effective.ClaudeAccountUUID = ""
	effective.ClaudeOrgUUID = ""
	effective.ClaudeDeviceID = ""
	effective.ClaudeScopes = ""
	effective.ClaudeRefreshExpiresAt = 0
	clearReq := ClaudeRefreshRequest{Stored: cloneCreds(stored), Effective: cloneCreds(effective), Replacement: true, Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode}
	cleared, err := svc.RefreshSavedClaude(context.Background(), 3, clearReq)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.AccessToken == "" || cleared.ClaudeAccountUUID != "" || cleared.ClaudeScopes != "" || cleared.ClaudeDeviceID == "" || cleared.ClaudeDeviceID == stored.ClaudeDeviceID {
		t.Fatalf("replacement borrowed identity: %+v", cleared)
	}
	if cleared.TokenURL != stored.TokenURL || cleared.ClientID != stored.ClientID {
		t.Fatalf("replacement migrated config: %+v", cleared)
	}

	conflictStored := cloneCreds(stored)
	conflictStored.TokenURL = conflictSrv.URL
	st.creds[3] = cloneCreds(conflictStored)
	conflictEffective := cloneCreds(effective)
	conflictEffective.TokenURL = conflictSrv.URL
	conflictReq := ClaudeRefreshRequest{Stored: conflictStored, Effective: conflictEffective, Replacement: true, Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode}
	conflictDone := make(chan error, 1)
	go func() {
		_, err := svc.RefreshSavedClaude(context.Background(), 3, conflictReq)
		conflictDone <- err
	}()
	waitN(t, entered, 1)
	reconnect := cloneCreds(stored)
	reconnect.AccessToken = "reconnect-access"
	reconnect.RefreshToken = "reconnect-refresh"
	st.mu.Lock()
	st.creds[3] = reconnect
	st.mu.Unlock()
	close(release)
	if err := <-conflictDone; !IsCredentialConflict(err) {
		t.Fatalf("replacement conflict err = %v", err)
	}
	st.mu.Lock()
	final := st.creds[3]
	st.mu.Unlock()
	if final.AccessToken != "reconnect-access" || final.RefreshToken != "reconnect-refresh" {
		t.Fatalf("replacement clobbered reconnect: %+v", final)
	}
}

func TestSavedClaudeRefreshKeepsDeviceInitialization(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"rotated-access","expires_in":3600}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	stored.ClaudeDeviceID = ""
	st.creds[4] = cloneCreds(stored)
	initialized := cloneCreds(stored)
	initialized.ClaudeDeviceID = repeatHex("ef")
	st.creds[4] = initialized
	svc := New(st)
	got, err := svc.RefreshSavedClaude(context.Background(), 4, ClaudeRefreshRequest{
		Stored: cloneCreds(stored), Effective: cloneCreds(stored),
		Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "rotated-access" || got.ClaudeDeviceID != repeatHex("ef") || got.ClaudeAccountUUID != stored.ClaudeAccountUUID {
		t.Fatalf("device initialization lost: %+v", got)
	}
}

func TestSavedClaudeRefreshRequiresAtomicStoreBeforeTokenCall(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"would-rotate","expires_in":3600}`
	})
	stored := savedClaudeBasis(srv.URL)
	st := &plainStore{creds: cloneCreds(stored)}
	svc := New(st)
	got, err := svc.RefreshSavedClaude(context.Background(), 1, ClaudeRefreshRequest{
		Stored: cloneCreds(stored), Effective: cloneCreds(stored),
		Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
	})
	if err == nil || IsCredentialConflict(err) || IsInvalidGrant(err) || !strings.Contains(err.Error(), "atomic") {
		t.Fatalf("missing provider-aware CAS err = %v", err)
	}
	if got != nil {
		t.Fatalf("missing provider-aware CAS returned credentials: %+v", got)
	}
	if hits.Load() != 0 {
		t.Fatalf("token endpoint called %d times before the capability check", hits.Load())
	}
	if st.writes != 0 {
		t.Fatalf("store writes = %d, want 0", st.writes)
	}
}

// plainStore has the legacy whole-blob and blob-only CAS capabilities but not
// the provider-aware conditional write. A saved Claude refresh must fail before
// it rotates the refresh token.
type plainStore struct {
	creds  *domain.OAuthCreds
	writes int
}

func (p *plainStore) GetProvider(context.Context, int64) (*domain.Provider, error) {
	cp := *p.creds
	return &domain.Provider{ID: 1, Protocol: domain.ProtocolClaudeCode, AuthMethod: domain.AuthOAuth, OAuthCreds: &cp}, nil
}

func (p *plainStore) UpdateProviderOAuth(context.Context, int64, *domain.OAuthCreds) error {
	p.writes++
	return nil
}

func (p *plainStore) UpdateProviderOAuthCAS(context.Context, int64, func(*domain.OAuthCreds) (*domain.OAuthCreds, bool, error)) (*domain.OAuthCreds, error) {
	p.writes++
	return nil, errors.New("blob-only CAS is not enough for a saved Claude refresh")
}

func TestSavedClaudeRefreshRejectsConnectionSwitchDuringCall(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv, _ := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 200, `{"access_token":"obsolete-access","expires_in":3600}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[5] = cloneCreds(stored)
	svc := New(st)
	done := make(chan struct {
		creds *domain.OAuthCreds
		err   error
	}, 1)
	go func() {
		got, err := svc.RefreshSavedClaude(context.Background(), 5, ClaudeRefreshRequest{
			Stored: cloneCreds(stored), Effective: cloneCreds(stored),
			Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
		})
		done <- struct {
			creds *domain.OAuthCreds
			err   error
		}{got, err}
	}()
	waitN(t, entered, 1)

	st.setConnection(5, domain.AuthOAuth, domain.ProtocolAnthropic)
	close(release)
	got := <-done
	if !IsCredentialConflict(got.err) || got.creds != nil {
		t.Fatalf("connection switch: creds %+v err %v, want conflict", got.creds, got.err)
	}
	st.mu.Lock()
	final := st.creds[5]
	st.mu.Unlock()
	if final.AccessToken != "stored-access" || final.ExpiresAt != 11 {
		t.Fatalf("connection switch received stale rotation: %+v", final)
	}
}

func TestSavedClaudeRefreshMapsDeletionToConflict(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv, _ := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 200, `{"access_token":"obsolete-access","expires_in":3600}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[6] = cloneCreds(stored)
	svc := New(st)
	done := make(chan error, 1)
	go func() {
		_, err := svc.RefreshSavedClaude(context.Background(), 6, ClaudeRefreshRequest{
			Stored: cloneCreds(stored), Effective: cloneCreds(stored),
			Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
		})
		done <- err
	}()
	waitN(t, entered, 1)

	st.mu.Lock()
	delete(st.creds, 6)
	st.mu.Unlock()
	close(release)
	if err := <-done; !IsCredentialConflict(err) {
		t.Fatalf("deletion during refresh err = %v, want conflict", err)
	}
	if st.writes.Load() != 0 {
		t.Fatalf("deletion during refresh wrote %d times", st.writes.Load())
	}
}

func TestSavedClaudeRefreshDeletionOverridesUpstreamFailure(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv, _ := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 400, `{"error":"invalid_grant","error_description":"refresh token is invalid"}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[7] = cloneCreds(stored)
	svc := New(st)
	done := make(chan error, 1)
	go func() {
		_, err := svc.RefreshSavedClaude(context.Background(), 7, ClaudeRefreshRequest{
			Stored: cloneCreds(stored), Effective: cloneCreds(stored),
			Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
		})
		done <- err
	}()
	waitN(t, entered, 1)

	st.mu.Lock()
	delete(st.creds, 7)
	st.mu.Unlock()
	close(release)
	err := <-done
	if !IsCredentialConflict(err) || IsInvalidGrant(err) {
		t.Fatalf("deleted row classification err = %v, want conflict, not reconnect required", err)
	}
}

func TestSavedClaudeRefreshStoreFailureKeepsUpstreamError(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv, _ := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 400, `{"error":"invalid_grant","error_description":"refresh token is invalid"}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[8] = cloneCreds(stored)
	svc := New(st)
	done := make(chan error, 1)
	go func() {
		_, err := svc.RefreshSavedClaude(context.Background(), 8, ClaudeRefreshRequest{
			Stored: cloneCreds(stored), Effective: cloneCreds(stored),
			Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
		})
		done <- err
	}()
	waitN(t, entered, 1)

	st.mu.Lock()
	st.getErr = errors.New("db unavailable")
	st.mu.Unlock()
	close(release)
	err := <-done
	if IsCredentialConflict(err) || !IsInvalidGrant(err) {
		t.Fatalf("store failure masked upstream error: %v", err)
	}
}

func TestSavedClaudeRefreshRejectsChangedDeviceDuringCall(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv, _ := blockingTokenServer(t, entered, release, func(url.Values) (int, string) {
		return 200, `{"access_token":"obsolete-access","expires_in":3600}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[9] = cloneCreds(stored)
	svc := New(st)
	done := make(chan error, 1)
	go func() {
		_, err := svc.RefreshSavedClaude(context.Background(), 9, ClaudeRefreshRequest{
			Stored: cloneCreds(stored), Effective: cloneCreds(stored),
			Method: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
		})
		done <- err
	}()
	waitN(t, entered, 1)

	// A replaced device is a different install identity, not first-use
	// initialization of an empty one.
	replaced := cloneCreds(stored)
	replaced.ClaudeDeviceID = repeatHex("cd")
	st.mu.Lock()
	st.creds[9] = replaced
	st.mu.Unlock()
	close(release)
	if err := <-done; !IsCredentialConflict(err) {
		t.Fatalf("replaced device err = %v, want conflict", err)
	}
	st.mu.Lock()
	final := st.creds[9]
	st.mu.Unlock()
	if final.ClaudeDeviceID != repeatHex("cd") || final.AccessToken != "stored-access" {
		t.Fatalf("replaced device row = %+v", final)
	}
}

func savedClaudeBasis(tokenURL string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthManual, Preset: "custom", ClaudeCodeAuth: true,
		AccessToken: "stored-access", RefreshToken: "stored-refresh", ExpiresAt: 11,
		Email: "stored@example.com", TokenURL: tokenURL, ClientID: "stored-client",
		ClaudeAccountUUID: "acct-stored", AccountID: "acct-stored", ClaudeOrgUUID: "org-stored",
		ClaudeDeviceID: repeatHex("ab"), ClaudeScopes: "user:inference", ClaudeRefreshExpiresAt: 99,
	}
}

// A saved row that no longer uses the Claude protocol but still carries the
// Claude credential marker refreshes through the historical authoritative
// write, as it did before the saved Claude refresh path existed.
func TestRefreshAndPersistClaudeCredsOutsideClaudeProtocol(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"rotated-access","expires_in":3600}`
	})
	st := newFakeStore()
	stored := savedClaudeBasis(srv.URL)
	st.creds[1] = cloneCreds(stored)
	svc := New(st)
	got, err := svc.RefreshAndPersist(context.Background(), 1, cloneCreds(stored))
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "rotated-access" || got.ClaudeDeviceID != stored.ClaudeDeviceID {
		t.Fatalf("authoritative Claude refresh lost config: %+v", got)
	}
	st.mu.Lock()
	persisted := st.creds[1]
	st.mu.Unlock()
	if persisted.AccessToken != "rotated-access" || persisted.RefreshToken != stored.RefreshToken {
		t.Fatalf("authoritative Claude refresh not persisted: %+v", persisted)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
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
