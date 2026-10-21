package oauth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/store"
)

// httpClient is the client used for OAuth token endpoint calls (refresh and the
// connect exchange). It is shared across the package. Connect still uses the
// request context; coalesced Resolve refreshes use a service-owned timeout.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// refreshTimeout bounds a coalesced Resolve refresh: token-endpoint HTTP plus
// persistence. The package HTTP client already times out at 30s; the extra
// slack covers store writeback without leaving the detached goroutine
// unbounded if the database hangs.
const refreshTimeout = 45 * time.Second

// ProviderStore is the subset of the store the service needs: loading a provider
// to read its current OAuth creds, and persisting refreshed creds. Defining it
// here keeps oauth free of a store import (and import cycle) and makes the
// service testable with a fake.
type ProviderStore interface {
	GetProvider(ctx context.Context, id int64) (*domain.Provider, error)
	UpdateProviderOAuth(ctx context.Context, id int64, creds *domain.OAuthCreds) error
}

// OAuthCredsCASStore is the optional safe-write capability Claude Code needs.
// A Claude refresh or first-use device initialization must not overwrite a
// reconnect that committed after the token was read. Stores without this
// capability fail the Claude operation explicitly rather than doing a lossy
// whole-row write.
type OAuthCredsCASStore interface {
	UpdateProviderOAuthCAS(ctx context.Context, id int64, update func(current *domain.OAuthCreds) (*domain.OAuthCreds, bool, error)) (*domain.OAuthCreds, error)
}

// OAuthProviderCASStore is the provider-aware atomic capability a saved Claude
// dashboard refresh needs. The write is conditioned on the encrypted credential
// blob plus the provider auth method and protocol observed at the same read, so
// a concurrent connection switch cannot receive a stale rotation. The callback
// receives that connection state and rejects an incompatible one.
type OAuthProviderCASStore interface {
	UpdateProviderConnectionCAS(ctx context.Context, id int64, update func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error)) (*domain.OAuthCreds, error)
}

// ErrCredentialConflict reports that a dashboard Claude refresh observed a
// newer credential row and discarded its token-endpoint result. It is not an
// upstream rejection and must not be rendered as a successful refresh or as a
// fresh reconnect requirement.
var ErrCredentialConflict = errors.New("oauth: credentials changed during refresh")

// IsCredentialConflict reports whether err is ErrCredentialConflict.
func IsCredentialConflict(err error) bool { return errors.Is(err, ErrCredentialConflict) }

// ClaudeRefreshRequest is one saved Claude-protocol dashboard refresh. The
// row may use specialized Claude credentials (ClaudeCodeAuth) or generic OAuth
// credentials; the stored and effective markers must agree. Stored is the
// connection snapshot read before the token call. Effective is the grant sent
// to the token endpoint. Replacement is true only when the user pasted a
// different nonempty refresh token. Intent is fixed from that starting pair.
// It is not inferred by comparing Effective with a row read after the call.
// Method must be the raw explicit stored method and Protocol the stored
// protocol; an empty value is not treated as OAuth. The atomic write matches
// them, so a concurrent connection switch is a conflict.
type ClaudeRefreshRequest struct {
	Stored      *domain.OAuthCreds
	Effective   *domain.OAuthCreds
	Replacement bool
	Method      domain.AuthMethod
	Protocol    domain.Protocol
}

// connection returns the connection state the refresh was started from. The
// protocol falls back to the saved Claude default; the method does not. Raw
// empty method is effective APIKey and must not rotate tokens as OAuth.
func (r ClaudeRefreshRequest) connection() (domain.AuthMethod, domain.Protocol) {
	proto := r.Protocol
	if proto == "" {
		proto = domain.ProtocolClaudeCode
	}
	return r.Method, proto
}

// Service resolves an effective upstream bearer token for a provider, refreshing
// OAuth access tokens as needed. It is the single entry point the proxy and
// dashboard probes call before contacting an upstream.
type Service struct {
	store ProviderStore
	// now is overridable in tests; production uses time.Now.
	now func() time.Time

	mu sync.Mutex
	// inflight deduplicates concurrent refreshes by provider id and mode:
	// forced (reactive 401/403) refreshes coalesce among themselves but are
	// never shared with proactive callers, whose cached result may reflect a
	// different outcome. Keyed on (provider id, force).
	inflight map[refreshKey]*call
	// testOnJoin, if set, is called after this caller has obtained the
	// inflight slot (leader or waiter) and before it waits. Tests use it as
	// a join barrier; production leaves it nil.
	testOnJoin func()
}

type refreshKey struct {
	id    int64
	force bool
}

type call struct {
	done chan struct{}

	mu    sync.Mutex
	creds *domain.OAuthCreds
	err   error
}

// publish stores the refresh outcome before close(done). Callers must publish
// then close so waiters that observe done never race with the write.
func (c *call) publish(creds *domain.OAuthCreds, err error) {
	c.mu.Lock()
	c.creds, c.err = creds, err
	c.mu.Unlock()
}

func (c *call) result() (*domain.OAuthCreds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creds, c.err
}

// New returns a Service backed by the given store.
func New(store ProviderStore) *Service {
	return &Service{store: store, now: time.Now, inflight: map[refreshKey]*call{}}
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
// requests. Resolve does not mutate the passed provider. Call ResolveCreds
// when the request also needs the credential snapshot that produced the token.
func (s *Service) Resolve(ctx context.Context, provider *domain.Provider, force bool) (string, error) {
	// Method is independent of scheme and protocol. An API-key row may still
	// carry config-only OAuthCreds (for example a Kiro profile). Those creds
	// must not replace the static key, even when AccessToken is populated.
	if provider == nil || provider.Method() != domain.AuthOAuth {
		if provider == nil {
			return "", nil
		}
		return provider.APIKey, nil
	}
	creds, err := s.ResolveCreds(ctx, provider, force)
	if creds != nil && creds.AccessToken != "" {
		return creds.AccessToken, err
	}
	return "", err
}

// ResolveCreds returns the credential snapshot that produced the effective
// token. The snapshot is a copy. The caller may store it on a request-local
// provider. ResolveCreds does not mutate the passed provider or a shared row
// except for the existing refresh and Claude device initialization writes.
func (s *Service) ResolveCreds(ctx context.Context, provider *domain.Provider, force bool) (*domain.OAuthCreds, error) {
	if provider == nil || provider.Method() != domain.AuthOAuth || provider.OAuthCreds == nil {
		if provider == nil || provider.OAuthCreds == nil {
			return nil, nil
		}
		cp := *provider.OAuthCreds
		return &cp, nil
	}
	creds := provider.OAuthCreds

	// The caller's creds are a request-local snapshot taken at combo hydration.
	// A concurrent dashboard reconnect or edit may have replaced the stored
	// row since; refreshing the snapshot and persisting the result would
	// clobber the live row (old refresh token, old auxiliary fields) and can
	// double-use a rotated single-use refresh token. Prefer the live stored
	// creds as the basis. Best effort: on GetProvider error fall back to the
	// snapshot, matching the best-available-token philosophy of the failure
	// paths below.
	if live, err := s.store.GetProvider(ctx, provider.ID); err == nil && live != nil && live.OAuthCreds != nil {
		creds = live.OAuthCreds
	}
	// Every non-force Resolve consults shouldRefresh on the selected basis.
	// A live row that is no longer near expiry (a concurrent rotation already
	// persisted a fresh token) skips the token endpoint. The same skip applies
	// to a fresh, unknown-expiry, or non-refreshable snapshot when the live
	// row could not be read, so a store error does not force a token call.
	var updated *domain.OAuthCreds
	var resolveErr error
	if force || shouldRefresh(creds, s.now()) {
		// A proactive caller must not reuse a forced refresh's possibly-different
		// outcome, and vice versa; the (id, force) inflight key keeps them isolated
		// while concurrent forced refreshes (parallel 401 reactions on one
		// provider) coalesce instead of double-using a rotating refresh token.
		updated, resolveErr = s.dedupRefresh(ctx, refreshKey{id: provider.ID, force: force}, creds)
		if updated == nil {
			updated = creds
		}
	} else {
		updated = creds
	}
	ready, err := s.ensureClaudeIdentity(ctx, provider.ID, updated)
	if err != nil && resolveErr == nil {
		resolveErr = err
	}
	if ready == nil {
		ready = updated
	}
	// Pair the token this call resolved with the account and device that
	// belong to that same snapshot. A later store read must not replace one
	// without the others.
	cp := *ready
	return &cp, resolveErr
}

// ensureClaudeIdentity persists one device id for a Claude connection that
// does not have one yet. Compare-and-swap is the concurrency control: two
// service instances that share a store can each generate a candidate, but only
// the first write commits and the loser returns the winner's snapshot. A
// re-read alone is not a compare-and-swap. Randomness failure is returned and
// does not invent a device.
func (s *Service) ensureClaudeIdentity(ctx context.Context, id int64, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	if creds == nil || !creds.ClaudeCodeAuth || isDeviceID(creds.ClaudeDeviceID) {
		return creds, nil
	}
	return s.initClaudeDevice(ctx, id, creds)
}

// initClaudeDevice merges a generated device into the newest stored snapshot.
// The generated value is stable across CAS retries. A concurrent refresh,
// reconnect, or another service instance wins without losing its token,
// account, or device. A deleted row or an empty/replaced credential row fails
// closed instead of writing the caller's stale snapshot.
func (s *Service) initClaudeDevice(ctx context.Context, id int64, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	cas, ok := s.store.(OAuthCredsCASStore)
	if !ok {
		return creds, errors.New("oauth: store does not support atomic Claude device initialization")
	}
	generated := *creds
	if err := ensureClaudeDeviceID(&generated); err != nil {
		return creds, err
	}
	ready, err := cas.UpdateProviderOAuthCAS(ctx, id, func(current *domain.OAuthCreds) (*domain.OAuthCreds, bool, error) {
		if current == nil || !current.ClaudeCodeAuth {
			return nil, false, errors.New("oauth: Claude credential row was replaced or deleted")
		}
		if isDeviceID(current.ClaudeDeviceID) {
			return current, false, nil
		}
		next := *current
		next.ClaudeDeviceID = generated.ClaudeDeviceID
		return &next, true, nil
	})
	if err != nil {
		return creds, err
	}
	if ready == nil || !isDeviceID(ready.ClaudeDeviceID) {
		return creds, errors.New("oauth: Claude device initialization did not persist")
	}
	return ready, nil
}

// dedupRefresh collapses concurrent refreshes for the same provider and mode
// into one token-endpoint call. The first caller starts a detached refresh;
// later callers wait on its result. The shared work uses a service-owned
// timeout so a cancelled starter does not abort waiters or persistence.
// The inflight entry is removed after the result is published so a later
// request re-evaluates expiry against the now-current token.
func (s *Service) dedupRefresh(ctx context.Context, key refreshKey, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	c := s.startOrJoin(ctx, key, creds)
	if s.testOnJoin != nil {
		s.testOnJoin()
	}
	select {
	case <-c.done:
		return c.result()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// startOrJoin returns the inflight call for key, starting a detached refresh
// when none is running. Shared work uses WithoutCancel of the starter's ctx
// (keeps request values, drops cancellation) plus refreshTimeout so the token
// HTTP client (30s) and persistence cannot inherit a cancelled caller and are
// not left unbounded.
func (s *Service) startOrJoin(ctx context.Context, key refreshKey, creds *domain.OAuthCreds) *call {
	s.mu.Lock()
	if c, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		return c
	}
	c := &call{done: make(chan struct{})}
	s.inflight[key] = c
	s.mu.Unlock()

	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		updated, err := s.doRefresh(rctx, key.id, creds, false)
		c.publish(updated, err)
		close(c.done)
		s.mu.Lock()
		// Delete after close so a concurrent arrival either joins this finished
		// call (and reads the published result) or, once the slot is gone,
		// starts a new Resolve that re-checks live expiry instead of launching
		// a duplicate while this one is still visible.
		if cur := s.inflight[key]; cur == c {
			delete(s.inflight, key)
		}
		s.mu.Unlock()
	}()
	return c
}

// doRefresh performs a single refresh against the given creds basis. Claude
// Code uses compare-and-swap so a reconnect, another refresh, or a device
// initialization that commits after the basis read is preserved. Other
// providers keep their historical whole-row writeback; their refreshed fields
// are provider-specific and must not pass through a Claude-shaped merge.
// A saved Claude-protocol dashboard refresh does not use this method. It uses
// RefreshSavedClaude, which carries the starting stored snapshot separately
// from the effective grant and also guards the provider connection state. This
// method keeps the historical authoritative write for other rows whose creds
// still carry the Claude Code marker, for example after a protocol transition.
func (s *Service) doRefresh(ctx context.Context, id int64, creds *domain.OAuthCreds, authoritative bool) (*domain.OAuthCreds, error) {
	if !authoritative && creds != nil && creds.ClaudeCodeAuth {
		return s.doRefreshClaude(ctx, id, creds)
	}
	cp := *creds
	if err := refresh(ctx, &cp, s.now()); err != nil {
		return nil, err
	}
	if authoritative && cp.ClaudeCodeAuth {
		return s.persistClaudeAuthoritative(ctx, id, &cp, creds)
	}
	if err := s.store.UpdateProviderOAuth(ctx, id, &cp); err != nil {
		return nil, err
	}
	out := cp
	return &out, nil
}

// doRefreshClaude refreshes first, then writes only if the stored credential
// generation is still the one this call started from. The CAS callback applies
// the rotated token fields onto the current row, so a concurrently initialized
// device or a reconnect is not reverted. If another writer replaced the row,
// the caller receives that newer snapshot instead of a stale rotation.
func (s *Service) doRefreshClaude(ctx context.Context, id int64, basis *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	cas, ok := s.store.(OAuthCredsCASStore)
	if !ok {
		return nil, errors.New("oauth: store does not support atomic Claude refresh")
	}
	rotated := *basis
	if err := refresh(ctx, &rotated, s.now()); err != nil {
		// Another service instance may have rotated the same refresh token while
		// this call was in flight. Prefer the newly stored usable row over
		// surfacing a stale invalid_grant.
		if live, lerr := s.store.GetProvider(ctx, id); lerr == nil && live != nil && live.OAuthCreds != nil && claudeGenerationChanged(live.OAuthCreds, basis) {
			cp := *live.OAuthCreds
			return &cp, nil
		}
		return nil, err
	}
	return cas.UpdateProviderOAuthCAS(ctx, id, func(current *domain.OAuthCreds) (*domain.OAuthCreds, bool, error) {
		if current == nil || !current.ClaudeCodeAuth {
			return nil, false, errors.New("oauth: Claude credential row was replaced or deleted")
		}
		if current.AccessToken == rotated.AccessToken &&
			current.RefreshToken == rotated.RefreshToken &&
			current.ExpiresAt == rotated.ExpiresAt {
			return current, false, nil
		}
		if claudeGenerationChanged(current, basis) {
			return current, false, nil
		}
		next := mergeClaudeRefresh(current, &rotated)
		return next, true, nil
	})
}

// persistClaudeAuthoritative writes dashboard-refreshed credentials for a row
// whose creds carry the Claude Code marker but that does not use the saved
// Claude-protocol refresh path, for example a row moved to another protocol.
// The form is authoritative for token values, but the row may still have
// changed while the token endpoint was called. A CAS retry keeps the operation
// deterministic and avoids a torn credential blob.
func (s *Service) persistClaudeAuthoritative(ctx context.Context, id int64, rotated, basis *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	cas, ok := s.store.(OAuthCredsCASStore)
	if !ok {
		if err := s.store.UpdateProviderOAuth(ctx, id, rotated); err != nil {
			return nil, err
		}
		out := *rotated
		return &out, nil
	}
	return cas.UpdateProviderOAuthCAS(ctx, id, func(current *domain.OAuthCreds) (*domain.OAuthCreds, bool, error) {
		if current == nil {
			return nil, false, errors.New("oauth: Claude credential row was deleted")
		}
		next := *rotated
		sameGeneration := sameClaudeRefreshToken(current, basis)
		sameAccount := next.ClaudeAccountUUID != "" && current.ClaudeAccountUUID != "" &&
			next.ClaudeAccountUUID == current.ClaudeAccountUUID
		if !isDeviceID(next.ClaudeDeviceID) && isDeviceID(current.ClaudeDeviceID) && (sameAccount || sameGeneration) {
			next.ClaudeDeviceID = current.ClaudeDeviceID
		}
		// Borrow identity only while the stored row still belongs to the refresh
		// basis. A different pasted refresh token is an unknown grant, even when
		// the token response omitted account and organization.
		if next.ClaudeAccountUUID == "" && current.ClaudeAccountUUID != "" && sameGeneration {
			next.ClaudeAccountUUID = current.ClaudeAccountUUID
			next.AccountID = current.ClaudeAccountUUID
		}
		if next.ClaudeOrgUUID == "" && current.ClaudeOrgUUID != "" && next.ClaudeAccountUUID != "" &&
			next.ClaudeAccountUUID == current.ClaudeAccountUUID {
			next.ClaudeOrgUUID = current.ClaudeOrgUUID
			if next.OrganizationID == "" {
				next.OrganizationID = current.OrganizationID
			}
		}
		return &next, true, nil
	})
}

// persistSavedClaudeRefresh writes a dashboard refresh only while the stored
// row still matches the snapshot read before the token call. A reconnect,
// competing rotation, config change, deletion, or incompatible auth/protocol
// switch rejects the result. A first-use device initialization is not a
// conflict: the rotated fields are applied onto that newer device. There is no
// whole-blob fallback. Generic OAuth credentials keep their generic wire result,
// including IDToken and derived claims, instead of the Claude-only merge.
func (s *Service) persistSavedClaudeRefresh(ctx context.Context, id int64, req ClaudeRefreshRequest, rotated *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	cas, ok := s.store.(OAuthProviderCASStore)
	if !ok {
		return nil, errors.New("oauth: store does not support atomic Claude refresh")
	}
	marked := req.Stored.ClaudeCodeAuth
	method, proto := req.connection()
	out, err := cas.UpdateProviderConnectionCAS(ctx, id, func(current *domain.OAuthCreds, storedMethod domain.AuthMethod, storedProto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		if storedMethod != method || storedProto != proto {
			return nil, false, ErrCredentialConflict
		}
		if claudeRefreshBasisChanged(current, req.Stored) {
			return nil, false, ErrCredentialConflict
		}
		if marked {
			if req.Replacement {
				return savedClaudeReplacement(current, rotated), true, nil
			}
			return mergeClaudeRefresh(current, rotated), true, nil
		}
		if req.Replacement {
			return savedGenericReplacement(current, rotated), true, nil
		}
		return mergeGenericSavedRefresh(current, rotated), true, nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			return nil, ErrCredentialConflict
		}
		return nil, err
	}
	return out, nil
}

// claudeRefreshBasisChanged reports a stored change that makes a dashboard
// refresh obsolete. A first-use device initialization fills the empty device
// of the basis and is not a new generation. Any other device change is a
// different install identity and conflicts. Access-token and expiry changes
// count even when the refresh token is unchanged, because a competing rotation
// can retain that token.
func claudeRefreshBasisChanged(current, basis *domain.OAuthCreds) bool {
	if current == nil || basis == nil {
		return true
	}
	left := *current
	right := *basis
	if !isDeviceID(right.ClaudeDeviceID) && isDeviceID(left.ClaudeDeviceID) {
		left.ClaudeDeviceID = ""
		right.ClaudeDeviceID = ""
	}
	return !reflect.DeepEqual(left, right)
}

// savedClaudeReplacement keeps the current connection config and applies the
// replacement grant. It does not borrow the previous account, device, scopes,
// access token, or refresh expiry. A device that already belongs to a
// confirmed same account is kept.
func savedClaudeReplacement(current, rotated *domain.OAuthCreds) *domain.OAuthCreds {
	next := *rotated
	if next.ExtraAuthParams != nil {
		next.ExtraAuthParams = cloneStringMap(rotated.ExtraAuthParams)
	}
	applySavedOAuthConfig(&next, current)
	if rotated.ClaudeAccountUUID != "" && current.ClaudeAccountUUID == rotated.ClaudeAccountUUID && isDeviceID(current.ClaudeDeviceID) {
		next.ClaudeDeviceID = current.ClaudeDeviceID
	}
	return &next
}

// savedGenericReplacement applies a replacement generic grant. Generic OAuth
// has no confirmed Claude account marker, so the previous device is not kept.
// The caller clears old grant identity on the effective grant; the returned
// token fields stay authoritative.
func savedGenericReplacement(current, rotated *domain.OAuthCreds) *domain.OAuthCreds {
	next := *rotated
	if next.ExtraAuthParams != nil {
		next.ExtraAuthParams = cloneStringMap(rotated.ExtraAuthParams)
	}
	applySavedOAuthConfig(&next, current)
	return &next
}

// applySavedOAuthConfig copies connection configuration from stored onto next.
// Token, identity, device, granted scopes, and refresh expiry stay on next.
func applySavedOAuthConfig(next, stored *domain.OAuthCreds) {
	next.Mode = stored.Mode
	next.Preset = stored.Preset
	next.AuthURL = stored.AuthURL
	next.TokenURL = stored.TokenURL
	next.ClientID = stored.ClientID
	next.ClientSecret = stored.ClientSecret
	next.Scopes = stored.Scopes
	next.RedirectURI = stored.RedirectURI
	next.PKCE = stored.PKCE
	next.ExtraAuthParams = cloneStringMap(stored.ExtraAuthParams)
	next.RefreshJSON = stored.RefreshJSON
	next.RefreshURL = stored.RefreshURL
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// claudeGenerationChanged reports that the stored row no longer belongs to the
// refresh basis. Device-only changes from first-use initialization are not a
// new generation. A different refresh token or Claude account is. Dashboard
// refresh uses claudeRefreshBasisChanged, which rejects every other stored
// change, including a same-token rotation.
func claudeGenerationChanged(current, basis *domain.OAuthCreds) bool {
	if current == nil || basis == nil {
		return true
	}
	if current.RefreshToken != basis.RefreshToken {
		return true
	}
	if current.ClaudeAccountUUID != basis.ClaudeAccountUUID {
		return true
	}
	return false
}

// sameClaudeRefreshToken reports that the stored row still carries the refresh
// token the caller refreshed. Account fields may be empty on one side; they do
// not establish a new credential generation for this borrow decision.
func sameClaudeRefreshToken(current, basis *domain.OAuthCreds) bool {
	if current == nil || basis == nil || basis.RefreshToken == "" {
		return false
	}
	return current.RefreshToken == basis.RefreshToken
}

// mergeClaudeRefresh starts from the newest stored row and applies only the
// rotated token fields. Device and any field the refresh response omitted stay
// at their freshly stored values. Connection config stays on the current row.
func mergeClaudeRefresh(current, rotated *domain.OAuthCreds) *domain.OAuthCreds {
	out := *current
	out.AccessToken = rotated.AccessToken
	out.ExpiresAt = rotated.ExpiresAt
	if rotated.RefreshToken != "" {
		out.RefreshToken = rotated.RefreshToken
	}
	if rotated.ClaudeRefreshExpiresAt != 0 {
		out.ClaudeRefreshExpiresAt = rotated.ClaudeRefreshExpiresAt
	}
	if rotated.ClaudeScopes != "" {
		out.ClaudeScopes = rotated.ClaudeScopes
	}
	if rotated.ClaudeAccountUUID != "" {
		out.ClaudeAccountUUID = rotated.ClaudeAccountUUID
		out.AccountID = rotated.AccountID
	}
	if rotated.ClaudeOrgUUID != "" {
		out.ClaudeOrgUUID = rotated.ClaudeOrgUUID
		if rotated.OrganizationID != "" {
			out.OrganizationID = rotated.OrganizationID
		}
	}
	if rotated.Email != "" {
		out.Email = rotated.Email
	}
	if !isDeviceID(out.ClaudeDeviceID) && isDeviceID(rotated.ClaudeDeviceID) {
		out.ClaudeDeviceID = rotated.ClaudeDeviceID
	}
	return &out
}

// mergeGenericSavedRefresh keeps the generic refresh mutation. The effective
// grant copy is the basis, so IDToken and email/account claims returned by the
// token endpoint are not dropped. Stored config is restored from the current
// row and a first-use device initialization is preserved.
func mergeGenericSavedRefresh(current, rotated *domain.OAuthCreds) *domain.OAuthCreds {
	out := *rotated
	if rotated.ExtraAuthParams != nil {
		out.ExtraAuthParams = cloneStringMap(rotated.ExtraAuthParams)
	}
	applySavedOAuthConfig(&out, current)
	if !isDeviceID(out.ClaudeDeviceID) && isDeviceID(current.ClaudeDeviceID) {
		out.ClaudeDeviceID = current.ClaudeDeviceID
	}
	return &out
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

// RefreshAndPersist refreshes a saved provider's tokens and writes the rotated
// credentials back to the store. The caller supplies the grant that must be
// refreshed. Non-Claude providers keep that historical write. Saved Claude
// dashboard refresh uses RefreshSavedClaude so the starting stored snapshot
// stays distinct from the effective grant.
func (s *Service) RefreshAndPersist(ctx context.Context, id int64, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	return s.doRefresh(ctx, id, creds, true)
}

// RefreshSavedClaude refreshes one saved Claude-protocol connection. req.Stored
// is the row snapshot that bounds the later write. req.Effective is the grant
// sent to the token endpoint. A conflict during the call discards the rotated
// result and returns ErrCredentialConflict. The stored row is not changed on
// conflict. The atomic store capability is checked before the token call, so a
// store without it cannot rotate a refresh token it cannot safely persist. The
// credential marker selects the specialized Claude or generic OAuth wire and
// persistence shape; it is never inferred from the provider protocol.
func (s *Service) RefreshSavedClaude(ctx context.Context, id int64, req ClaudeRefreshRequest) (*domain.OAuthCreds, error) {
	if req.Stored == nil || req.Effective == nil {
		return nil, errors.New("oauth: saved refresh requires stored and effective credentials")
	}
	if req.Stored.ClaudeCodeAuth != req.Effective.ClaudeCodeAuth {
		return nil, errors.New("oauth: saved refresh credential markers disagree")
	}
	method, proto := req.connection()
	if method != domain.AuthOAuth || proto != domain.ProtocolClaudeCode {
		return nil, errors.New("oauth: saved Claude refresh requires an OAuth Claude provider row")
	}
	if _, ok := s.store.(OAuthProviderCASStore); !ok {
		return nil, errors.New("oauth: store does not support atomic Claude refresh")
	}
	rotated := *req.Effective
	if rotated.ExtraAuthParams != nil {
		rotated.ExtraAuthParams = cloneStringMap(req.Effective.ExtraAuthParams)
	}
	if req.Replacement && !req.Stored.ClaudeCodeAuth {
		clearReplacedGenericIdentity(&rotated)
	}
	if err := refresh(ctx, &rotated, s.now()); err != nil {
		// The upstream failure is stale when the row vanished or changed
		// connection during the call. Only a missing row or an observed change
		// is a conflict; other store failures must not hide the upstream error.
		live, lerr := s.store.GetProvider(ctx, id)
		switch {
		case errors.Is(lerr, store.ErrNotFound):
			return nil, ErrCredentialConflict
		case lerr != nil:
			return nil, err
		case live == nil || live.Method() != method || live.Protocol != proto || claudeRefreshBasisChanged(live.OAuthCreds, req.Stored):
			return nil, ErrCredentialConflict
		}
		return nil, err
	}
	return s.persistSavedClaudeRefresh(ctx, id, req, &rotated)
}

// clearReplacedGenericIdentity prevents a replacement generic grant from
// inheriting token or identity values that the caller left on the effective
// snapshot. Token endpoint values overwrite these fields after the call.
func clearReplacedGenericIdentity(c *domain.OAuthCreds) {
	c.AccessToken = ""
	c.ExpiresAt = 0
	c.Email = ""
	c.IDToken = ""
	c.AccountID = ""
	c.OrganizationID = ""
	c.ClaudeAccountUUID = ""
	c.ClaudeOrgUUID = ""
	c.ClaudeDeviceID = ""
	c.ClaudeScopes = ""
	c.ClaudeRefreshExpiresAt = 0
}

// IsInvalidGrant reports whether err is an ErrInvalidGrant, for callers that
// want to surface a "reconnect required" state.
func IsInvalidGrant(err error) bool { return errors.Is(err, ErrInvalidGrant) }

// IsCursorNotRotatable reports a Cursor session that cannot exchange but still
// has a usable access token. Callers must not treat this as reconnect.
func IsCursorNotRotatable(err error) bool { return errors.Is(err, ErrCursorNotRotatable) }
