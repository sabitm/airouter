package oauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/store"
	"airouter/internal/xaigrant"
)

func xaiInvolved(p *domain.Provider) bool {
	return p != nil && xaigrant.IsGrant(p.OAuthCreds)
}

func xaiRefreshable(p *domain.Provider) bool {
	return p != nil && p.Method() == domain.AuthOAuth && xaigrant.IsGrant(p.OAuthCreds)
}

func (s *Service) storedXAIProvider(ctx context.Context, id int64) (*domain.Provider, bool) {
	if s == nil || s.store == nil || id == 0 {
		return nil, false
	}
	live, err := s.store.GetProvider(ctx, id)
	if err != nil || !xaiRefreshable(live) {
		return nil, false
	}
	return live, true
}

func (s *Service) storedXAIGrant(ctx context.Context, id int64) bool {
	_, ok := s.storedXAIProvider(ctx, id)
	return ok
}

// xaiSubmittedOverrides reports a form value that would move a saved xAI
// refresh off the stored token endpoint, client, preset, or generation. Blank
// fields are not overrides; the stored grant remains the basis.
func xaiSubmittedOverrides(submitted, stored *domain.OAuthCreds) bool {
	if submitted == nil || stored == nil {
		return false
	}
	if raw := strings.TrimSpace(submitted.TokenURL); raw != "" {
		norm, ok := xaigrant.NormalizeTokenURL(raw)
		if !ok || norm != xaigrant.TokenURL {
			return true
		}
	}
	if id := strings.TrimSpace(submitted.ClientID); id != "" && id != xaigrant.ClientID {
		return true
	}
	if preset := strings.TrimSpace(submitted.Preset); preset != "" && preset != xaigrant.Preset {
		return true
	}
	if generation := strings.TrimSpace(submitted.XAIGrantGeneration); generation != "" && stored.XAIGrantGeneration != "" && generation != stored.XAIGrantGeneration {
		return true
	}
	return false
}

func (s *Service) resolveXAI(ctx context.Context, provider *domain.Provider, force bool) (*domain.OAuthCreds, error) {
	if !xaiRefreshable(provider) {
		return nil, ErrCredentialConflict
	}
	live, err := s.loadXAI(ctx, provider.ID)
	if err != nil {
		return nil, err
	}
	if xaiGenerationConflicts(live.OAuthCreds, provider.OAuthCreds) || !sameXAIPrincipal(live.OAuthCreds, provider.OAuthCreds) {
		return nil, ErrCredentialConflict
	}
	// A forced retry of a token this grant already replaced reuses that rotation.
	// It must not submit the new grant or adopt a reconnect.
	if (!force && !shouldRefresh(live.OAuthCreds, s.now())) ||
		(force && xaiTokensChanged(live.OAuthCreds, provider.OAuthCreds) && sameXAIGeneration(live.OAuthCreds, provider.OAuthCreds)) {
		cp := *live.OAuthCreds
		return &cp, nil
	}
	basis := live.OAuthCreds
	updated, err := s.refreshXAIGrant(ctx, provider.ID, basis)
	if IsCredentialConflict(err) {
		if newer, loadErr := s.loadXAI(ctx, provider.ID); loadErr == nil &&
			sameXAIGeneration(newer.OAuthCreds, basis) && sameXAIPrincipal(newer.OAuthCreds, basis) &&
			xaiTokensChanged(newer.OAuthCreds, basis) {
			cp := *newer.OAuthCreds
			return &cp, nil
		}
	}
	return updated, err
}

func (s *Service) refreshXAIGrant(ctx context.Context, id int64, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	if creds == nil || creds.RefreshToken == "" {
		return nil, ErrInvalidGrant
	}
	key := refreshKey{
		id: id, xai: true,
		refreshGrant: sha256.Sum256([]byte(creds.RefreshToken)),
		generation:   sha256.Sum256([]byte(creds.XAIGrantGeneration)),
	}
	return s.dedupRefresh(ctx, key, creds)
}

func (s *Service) loadXAI(ctx context.Context, id int64) (*domain.Provider, error) {
	live, err := s.store.GetProvider(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && !xaiRefreshable(live) {
		return nil, ErrCredentialConflict
	}
	return live, err
}

// EnsureXAIGeneration initializes the random generation on a legacy grant. It
// is called only from the eligible Chat bridge path, after that path has proved
// it can sign. The callback preserves a generation that another writer already
// committed and never replaces token, account, or configuration fields.
func (s *Service) EnsureXAIGeneration(ctx context.Context, provider *domain.Provider) (*domain.OAuthCreds, error) {
	if provider == nil || !xaigrant.Official(provider) {
		return nil, ErrCredentialConflict
	}
	if provider.OAuthCreds == nil {
		return nil, ErrCredentialConflict
	}
	if provider.OAuthCreds.XAIGrantGeneration != "" {
		cp := *provider.OAuthCreds
		return &cp, nil
	}
	live, err := s.loadXAI(ctx, provider.ID)
	if err != nil {
		return nil, err
	}
	if !xaigrant.Official(live) || !sameXAIPrincipal(live.OAuthCreds, provider.OAuthCreds) {
		return nil, ErrCredentialConflict
	}
	if live.OAuthCreds.XAIGrantGeneration != "" {
		cp := *live.OAuthCreds
		return &cp, nil
	}
	minted, err := xaigrant.NewGeneration()
	if err != nil {
		return nil, err
	}
	cas, ok := s.store.(OAuthProviderCASStore)
	if !ok {
		return nil, errors.New("oauth: store does not support atomic xAI generation")
	}
	out, err := cas.UpdateProviderConnectionCAS(ctx, provider.ID, func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		if method != domain.AuthOAuth || proto != domain.ProtocolOpenAIResponses || !xaigrant.IsGrant(current) {
			return nil, false, ErrCredentialConflict
		}
		if current.XAIGrantGeneration != "" {
			return current, false, nil
		}
		next := *current
		next.XAIGrantGeneration = minted
		return &next, true, nil
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		return nil, ErrCredentialConflict
	}
	return out, err
}

func (s *Service) doRefreshXAI(ctx context.Context, id int64, basis *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	cas, ok := s.store.(OAuthProviderCASStore)
	if !ok {
		return nil, errors.New("oauth: store does not support atomic xAI refresh")
	}
	live, err := s.loadXAI(ctx, id)
	if err != nil {
		return nil, err
	}
	if xaiGenerationConflicts(live.OAuthCreds, basis) || !sameXAIPrincipal(live.OAuthCreds, basis) || xaiTokensChanged(live.OAuthCreds, basis) {
		return nil, ErrCredentialConflict
	}
	rotated := *live.OAuthCreds
	if err := refresh(ctx, &rotated, s.now()); err != nil {
		newer, loadErr := s.loadXAI(ctx, id)
		if IsCredentialConflict(loadErr) || loadErr == nil && xaiRefreshStale(newer.OAuthCreds, live.OAuthCreds) {
			return nil, ErrCredentialConflict
		}
		return nil, err
	}
	replacement := ""
	if !sameXAIPrincipal(&rotated, live.OAuthCreds) {
		replacement, err = xaigrant.NewGeneration()
		if err != nil {
			return nil, err
		}
	}
	out, err := cas.UpdateProviderConnectionCAS(ctx, id, func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		if method != domain.AuthOAuth || !xaigrant.IsGrant(current) {
			return nil, false, ErrCredentialConflict
		}
		if !sameXAIPrincipal(current, live.OAuthCreds) || xaiGenerationConflicts(current, live.OAuthCreds) || xaiTokensChanged(current, live.OAuthCreds) {
			return nil, false, ErrCredentialConflict
		}
		next := *current
		next.AccessToken = rotated.AccessToken
		next.RefreshToken = rotated.RefreshToken
		next.ExpiresAt = rotated.ExpiresAt
		if rotated.Email != "" {
			next.Email = rotated.Email
		}
		if rotated.IDToken != "" {
			next.IDToken = rotated.IDToken
		}
		if rotated.AccountID != "" {
			next.AccountID = rotated.AccountID
		}
		if replacement != "" {
			next.XAIGrantGeneration = replacement
		}
		return &next, true, nil
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		return nil, ErrCredentialConflict
	}
	return out, err
}

func sameXAIPrincipal(a, b *domain.OAuthCreds) bool {
	pa, pb := xaigrant.Principal(a), xaigrant.Principal(b)
	return pa == "" || pb == "" || pa == pb
}

func sameXAIGeneration(a, b *domain.OAuthCreds) bool {
	if a == nil || b == nil {
		return false
	}
	return a.XAIGrantGeneration != "" && a.XAIGrantGeneration == b.XAIGrantGeneration
}

func xaiGenerationConflicts(current, basis *domain.OAuthCreds) bool {
	if current == nil || basis == nil {
		return true
	}
	if current.XAIGrantGeneration == "" {
		return basis.XAIGrantGeneration != ""
	}
	if basis.XAIGrantGeneration == "" {
		return false
	}
	return current.XAIGrantGeneration != basis.XAIGrantGeneration
}

func xaiTokensChanged(current, basis *domain.OAuthCreds) bool {
	if current == nil || basis == nil {
		return true
	}
	return current.AccessToken != basis.AccessToken || current.RefreshToken != basis.RefreshToken || current.ExpiresAt != basis.ExpiresAt
}

func xaiRefreshStale(current, basis *domain.OAuthCreds) bool {
	return !sameXAIPrincipal(current, basis) || xaiGenerationConflicts(current, basis) || xaiTokensChanged(current, basis)
}
