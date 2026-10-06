package kirocatalog

import (
	"context"
	"errors"

	"airouter/internal/domain"
	"airouter/internal/oauth"
)

// ResolveToken verifies stored credential evidence before and after Resolve.
// Submitted Kiro configuration stays request-local. Account replacement must
// not attach a current token to a previous account's profile/configuration.
func ResolveToken(ctx context.Context, service *oauth.Service, store oauth.ProviderStore, probe *domain.Provider, force bool) (string, error) {
	if service == nil || store == nil || probe == nil || probe.OAuthCreds == nil {
		return "", errors.New("Kiro catalog identity unavailable")
	}
	before := *probe.OAuthCreds
	live, err := store.GetProvider(ctx, probe.ID)
	if err != nil || !sameCredentialBinding(probe, live) ||
		before.AccessToken != live.OAuthCreds.AccessToken || before.RefreshToken != live.OAuthCreds.RefreshToken {
		return "", errors.New("Kiro catalog identity changed")
	}
	baseline := cloneProvider(live)
	tok, err := service.Resolve(ctx, probe, force)
	if err != nil {
		return "", err
	}
	live, err = store.GetProvider(ctx, probe.ID)
	if err != nil || !sameCredentialBinding(probe, live) || !sameStoredConfig(baseline, live) || live.OAuthCreds.AccessToken != tok ||
		(!force && before.ExpiresAt == 0 && (before.AccessToken != tok || before.RefreshToken != live.OAuthCreds.RefreshToken)) ||
		(before.RefreshToken == "" && before.AccessToken != tok) ||
		(before.RefreshToken != "" && live.OAuthCreds.RefreshToken == "") {
		return "", errors.New("Kiro catalog identity changed")
	}
	creds := *probe.OAuthCreds
	creds.AccessToken, creds.RefreshToken, creds.ExpiresAt = tok, live.OAuthCreds.RefreshToken, live.OAuthCreds.ExpiresAt
	probe.OAuthCreds = &creds
	return tok, nil
}

func sameStoredConfig(a, b *domain.Provider) bool {
	if a == nil || b == nil || a.OAuthCreds == nil || b.OAuthCreds == nil {
		return false
	}
	return a.BaseURL == b.BaseURL && a.Auth() == b.Auth() &&
		a.OAuthCreds.ProfileArn == b.OAuthCreds.ProfileArn && a.OAuthCreds.Region == b.OAuthCreds.Region &&
		a.OAuthCreds.KiroTransport == b.OAuthCreds.KiroTransport && a.OAuthCreds.KiroDiscovery == b.OAuthCreds.KiroDiscovery
}

func sameCredentialBinding(a, b *domain.Provider) bool {
	if a == nil || b == nil || a.OAuthCreds == nil || b.OAuthCreds == nil || b.Protocol != domain.ProtocolKiro || a.Method() != b.Method() {
		return false
	}
	left, right := a.OAuthCreds, b.OAuthCreds
	return left.AccountID == right.AccountID && left.KiroAuth == right.KiroAuth && left.KiroIDP == right.KiroIDP &&
		left.ClientID == right.ClientID && left.ClientSecret == right.ClientSecret &&
		left.TokenURL == right.TokenURL && left.RefreshURL == right.RefreshURL
}
