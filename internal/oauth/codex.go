package oauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"airouter/internal/codexgrant"
	"airouter/internal/domain"
	"airouter/internal/store"
)

func codexInvolved(p *domain.Provider) bool {
	if p == nil {
		return false
	}
	if codexgrant.IsGrant(p.OAuthCreds) {
		return true
	}
	if p.Protocol != domain.ProtocolOpenAICodex || p.OAuthCreds == nil {
		return false
	}
	return p.OAuthCreds.Preset == codexgrant.Preset || codexgrant.Mixed(p.OAuthCreds)
}

func codexRefreshable(p *domain.Provider) bool {
	return p != nil && p.Protocol == domain.ProtocolOpenAICodex && p.Method() == domain.AuthOAuth && codexgrant.IsGrant(p.OAuthCreds)
}

func (s *Service) storedCodexProvider(ctx context.Context, id int64) (*domain.Provider, bool) {
	if s == nil || s.store == nil || id == 0 {
		return nil, false
	}
	live, err := s.store.GetProvider(ctx, id)
	if err != nil || !codexRefreshable(live) {
		return nil, false
	}
	return live, true
}

func (s *Service) storedCodexGrant(ctx context.Context, id int64) bool {
	_, ok := s.storedCodexProvider(ctx, id)
	return ok
}

// codexSubmittedOverrides reports a form value that would move a saved Codex
// refresh off the stored token endpoint, client, or preset. Blank fields are
// not overrides; the stored grant remains the basis.
func codexSubmittedOverrides(submitted, stored *domain.OAuthCreds) bool {
	if submitted == nil || stored == nil {
		return false
	}
	if raw := strings.TrimSpace(submitted.TokenURL); raw != "" {
		norm, ok := codexgrant.NormalizeTokenURL(raw)
		if !ok || norm != codexgrant.TokenURL {
			return true
		}
	}
	if id := strings.TrimSpace(submitted.ClientID); id != "" && id != codexgrant.ClientID {
		return true
	}
	if preset := strings.TrimSpace(submitted.Preset); preset != "" && preset != codexgrant.Preset {
		return true
	}
	if generation := strings.TrimSpace(submitted.CodexGrantGeneration); generation != "" && stored.CodexGrantGeneration != "" && generation != stored.CodexGrantGeneration {
		return true
	}
	return false
}

func (s *Service) resolveCodex(ctx context.Context, provider *domain.Provider, force bool) (*domain.OAuthCreds, error) {
	if provider == nil || provider.Protocol != domain.ProtocolOpenAICodex || !codexgrant.IsGrant(provider.OAuthCreds) {
		return nil, ErrCredentialConflict
	}
	live, err := s.loadCodex(ctx, provider.ID)
	if err != nil {
		return nil, err
	}
	if codexGenerationConflicts(live.OAuthCreds, provider.OAuthCreds) || !sameCodexAccount(live.OAuthCreds, provider.OAuthCreds) {
		return nil, ErrCredentialConflict
	}
	if live.OAuthCreds.CodexGrantGeneration == "" {
		ready, initErr := s.ensureCodexGeneration(ctx, provider.ID, live.OAuthCreds)
		if initErr != nil {
			return nil, initErr
		}
		live.OAuthCreds = ready
	}
	// A forced retry of a token this grant already replaced reuses that rotation.
	// It must not submit the new grant or adopt a reconnect.
	if (!force && !shouldRefresh(live.OAuthCreds, s.now())) ||
		(force && codexTokensChanged(live.OAuthCreds, provider.OAuthCreds) && sameCodexGeneration(live.OAuthCreds, provider.OAuthCreds)) {
		cp := *live.OAuthCreds
		return &cp, nil
	}
	basis := live.OAuthCreds
	updated, err := s.refreshCodexGrant(ctx, provider.ID, basis)
	if IsCredentialConflict(err) {
		if newer, loadErr := s.loadCodex(ctx, provider.ID); loadErr == nil &&
			sameCodexGeneration(newer.OAuthCreds, basis) && sameCodexAccount(newer.OAuthCreds, basis) &&
			codexTokensChanged(newer.OAuthCreds, basis) {
			cp := *newer.OAuthCreds
			return &cp, nil
		}
	}
	return updated, err
}

func (s *Service) refreshCodexGrant(ctx context.Context, id int64, creds *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	if creds == nil || creds.RefreshToken == "" {
		return nil, ErrInvalidGrant
	}
	key := refreshKey{
		id: id, codex: true,
		refreshGrant: sha256.Sum256([]byte(creds.RefreshToken)),
		generation:   sha256.Sum256([]byte(creds.CodexGrantGeneration)),
	}
	return s.dedupRefresh(ctx, key, creds)
}

func (s *Service) loadCodex(ctx context.Context, id int64) (*domain.Provider, error) {
	live, err := s.store.GetProvider(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && !codexRefreshable(live) {
		return nil, ErrCredentialConflict
	}
	return live, err
}

func (s *Service) ensureCodexGeneration(ctx context.Context, id int64, basis *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	if basis != nil && basis.CodexGrantGeneration != "" {
		cp := *basis
		return &cp, nil
	}
	minted, err := codexgrant.NewGeneration()
	if err != nil {
		return nil, err
	}
	cas, ok := s.store.(OAuthProviderCASStore)
	if !ok {
		return nil, errors.New("oauth: store does not support atomic Codex generation")
	}
	out, err := cas.UpdateProviderConnectionCAS(ctx, id, func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		if method != domain.AuthOAuth || proto != domain.ProtocolOpenAICodex || !codexgrant.IsGrant(current) {
			return nil, false, ErrCredentialConflict
		}
		if !sameCodexAccount(current, basis) || codexTokensChanged(current, basis) {
			return nil, false, ErrCredentialConflict
		}
		if current.CodexGrantGeneration != "" {
			return current, false, nil
		}
		next := *current
		next.CodexGrantGeneration = minted
		return &next, true, nil
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		return nil, ErrCredentialConflict
	}
	return out, err
}

func (s *Service) doRefreshCodex(ctx context.Context, id int64, basis *domain.OAuthCreds) (*domain.OAuthCreds, error) {
	cas, ok := s.store.(OAuthProviderCASStore)
	if !ok {
		return nil, errors.New("oauth: store does not support atomic Codex refresh")
	}
	live, err := s.loadCodex(ctx, id)
	if err != nil {
		return nil, err
	}
	if !sameCodexAccount(live.OAuthCreds, basis) || codexGenerationConflicts(live.OAuthCreds, basis) || codexTokensChanged(live.OAuthCreds, basis) {
		return nil, ErrCredentialConflict
	}
	if live.OAuthCreds.CodexGrantGeneration == "" {
		ready, initErr := s.ensureCodexGeneration(ctx, id, live.OAuthCreds)
		if initErr != nil {
			return nil, initErr
		}
		live.OAuthCreds = ready
		basis = ready
	}
	rotated := *live.OAuthCreds
	if err := refresh(ctx, &rotated, s.now()); err != nil {
		newer, loadErr := s.loadCodex(ctx, id)
		if IsCredentialConflict(loadErr) || loadErr == nil && codexRefreshStale(newer.OAuthCreds, basis) {
			return nil, ErrCredentialConflict
		}
		return nil, err
	}
	replacement := ""
	if rotated.AccountID != "" && rotated.AccountID != basis.AccountID {
		replacement, err = codexgrant.NewGeneration()
		if err != nil {
			return nil, err
		}
	}
	out, err := cas.UpdateProviderConnectionCAS(ctx, id, func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error) {
		if method != domain.AuthOAuth || proto != domain.ProtocolOpenAICodex || !codexgrant.IsGrant(current) {
			return nil, false, ErrCredentialConflict
		}
		if !sameCodexAccount(current, basis) || current.CodexGrantGeneration != basis.CodexGrantGeneration || codexTokensChanged(current, basis) {
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
			next.CodexGrantGeneration = replacement
		}
		return &next, true, nil
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		return nil, ErrCredentialConflict
	}
	return out, err
}

func sameCodexAccount(a, b *domain.OAuthCreds) bool {
	if a == nil || b == nil {
		return false
	}
	if a.AccountID == "" || b.AccountID == "" {
		return a.AccountID == b.AccountID
	}
	return a.AccountID == b.AccountID
}

func sameCodexGeneration(a, b *domain.OAuthCreds) bool {
	if a == nil || b == nil {
		return false
	}
	return a.CodexGrantGeneration != "" && a.CodexGrantGeneration == b.CodexGrantGeneration
}

func codexGenerationConflicts(current, basis *domain.OAuthCreds) bool {
	if current == nil || basis == nil {
		return true
	}
	if current.CodexGrantGeneration == "" || basis.CodexGrantGeneration == "" {
		return false
	}
	return current.CodexGrantGeneration != basis.CodexGrantGeneration
}

func codexTokensChanged(current, basis *domain.OAuthCreds) bool {
	if current == nil || basis == nil {
		return true
	}
	return current.AccessToken != basis.AccessToken || current.RefreshToken != basis.RefreshToken || current.ExpiresAt != basis.ExpiresAt
}

func codexRefreshStale(current, basis *domain.OAuthCreds) bool {
	return !sameCodexAccount(current, basis) || codexGenerationConflicts(current, basis) || codexTokensChanged(current, basis)
}
