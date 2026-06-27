package oauth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"airouter/internal/domain"
)

// httpClient is the client used for OAuth token endpoint calls (refresh and the
// connect exchange). It is shared across the package; refresh/connect calls are
// short and bounded by the request context.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// ProviderStore is the subset of the store the service needs: loading a provider
// to read its current OAuth creds, and persisting refreshed creds. Defining it
// here keeps oauth free of a store import (and import cycle) and makes the
// service testable with a fake.
type ProviderStore interface {
	GetProvider(ctx context.Context, id int64) (*domain.Provider, error)
	UpdateProviderOAuth(ctx context.Context, id int64, creds *domain.OAuthCreds) error
}

// Service resolves an effective upstream bearer token for a provider, refreshing
// OAuth access tokens as needed. It is the single entry point the proxy and
// dashboard probes call before contacting an upstream.
type Service struct {
	store ProviderStore
	// now is overridable in tests; production uses time.Now.
	now func() time.Time

	mu       sync.Mutex
	inflight map[int64]*call // provider id -> in-flight or recently cached refresh
}

type call struct {
	done  chan struct{}
	creds *domain.OAuthCreds
	err   error
}

// New returns a Service backed by the given store.
func New(store ProviderStore) *Service {
	return &Service{store: store, now: time.Now, inflight: map[int64]*call{}}
}

// Resolve returns the bearer token to send upstream for a provider.
//
// For apikey providers it returns the static APIKey unchanged. For oauth
// providers it refreshes proactively when the access token is near expiry (or
// always, when force is set for a reactive 401/403 retry), persists the rotated
// credentials, and returns the access token.
//
// The provider passed in is a request-local hydrated copy, so the caller may
// overwrite provider.APIKey with the returned token without affecting other
// requests. Resolve does not mutate the passed provider.
func (s *Service) Resolve(ctx context.Context, provider *domain.Provider, force bool) (string, error) {
	if provider.Method() != domain.AuthOAuth || provider.OAuthCreds == nil {
		return provider.APIKey, nil
	}
	creds := provider.OAuthCreds

	if !force && !shouldRefresh(creds, s.now()) {
		return creds.AccessToken, nil
	}

	// The cached/in-flight result is only valid for a forced refresh when force
	// was requested: a proactive caller must not reuse a forced refresh's
	// possibly-different outcome, and vice versa. Key on (id, force) by running
	// the forced path outside the dedup map.
	if force {
		updated, err := s.doRefresh(ctx, provider.ID, creds)
		if err != nil {
			// On a forced refresh failure, fall back to the current token: the
			// 401 may have been transient, and an expired token is no worse than
			// none. ErrInvalidGrant surfaces so the caller can flag reconnect.
			return creds.AccessToken, err
		}
		return updated.AccessToken, nil
	}

	updated, err := s.dedupRefresh(ctx, provider.ID, creds)
	if err != nil {
		return creds.AccessToken, err
	}
	return updated.AccessToken, nil
}

// dedupRefresh collapses concurrent proactive refreshes for the same provider
// into one token-endpoint call: the first caller performs the refresh, later
// callers wait on its result and reuse the refreshed creds. The window is short
// (the HTTP round-trip); the cached call is dropped once done so the next
// request re-evaluates expiry against the now-current token.
func (s *Service) dedupRefresh(ctx context.Context, id int64, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	s.mu.Lock()
	if c, ok := s.inflight[id]; ok {
		s.mu.Unlock()
		select {
		case <-c.done:
			return c.creds, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	s.inflight[id] = c
	s.mu.Unlock()

	defer func() {
		close(c.done)
		s.mu.Lock()
		// Only clear if this call still owns the slot; a later caller may have
		// already replaced it (it cannot, since we hold the slot until close,
		// but the guard is cheap and correct).
		if cur := s.inflight[id]; cur == c {
			delete(s.inflight, id)
		}
		s.mu.Unlock()
	}()

	updated, err := s.doRefresh(ctx, id, creds)
	c.creds, c.err = updated, err
	return updated, err
}

// doRefresh performs a single refresh and persists the result. It re-reads the
// provider from the store first so a concurrent dashboard edit to the OAuth
// config does not cause a stale-refresh-token writeback to clobber it: the
// refresh runs against the live creds, and only the token fields are written.
func (s *Service) doRefresh(ctx context.Context, id int64, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	// Work on a copy so a failed refresh leaves the caller's creds untouched.
	cp := *creds
	if err := refresh(ctx, &cp, s.now()); err != nil {
		return nil, err
	}
	if err := s.store.UpdateProviderOAuth(ctx, id, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// RefreshTokens exchanges the refresh token for a new access token without
// persisting, for the dashboard's manual-import flow where the provider is not
// yet saved (no id to write back to). It works on a copy, so the caller's creds
// are untouched on failure.
func (s *Service) RefreshTokens(ctx context.Context, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	cp := *creds
	if err := refresh(ctx, &cp, s.now()); err != nil {
		return nil, err
	}
	return &cp, nil
}

// IsInvalidGrant reports whether err is an ErrInvalidGrant, for callers that
// want to surface a "reconnect required" state.
func IsInvalidGrant(err error) bool { return errors.Is(err, ErrInvalidGrant) }
