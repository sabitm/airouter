package kirocatalog

import (
	"context"
	"errors"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/oauth"
)

type credentialStore struct {
	current     *domain.Provider
	reads       int
	changeAt    int
	replacement *domain.Provider
}

func (s *credentialStore) GetProvider(context.Context, int64) (*domain.Provider, error) {
	s.reads++
	if s.reads == s.changeAt {
		s.current = s.replacement
	}
	return cloneProvider(s.current), nil
}

func (s *credentialStore) UpdateProviderOAuth(context.Context, int64, *domain.OAuthCreds) error {
	return errors.New("unexpected refresh")
}

func credentialProvider(token string) *domain.Provider {
	return &domain.Provider{ID: 1, BaseURL: "https://catalog.example", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{AccessToken: token, AccountID: "account", ProfileArn: "arn:profile"}}
}

func TestResolveTokenRejectsConcurrentReplacement(t *testing.T) {
	for _, refreshable := range []bool{false, true} {
		for _, when := range []int{1, 2, 3} {
			old := credentialProvider("old")
			replacement := credentialProvider("new")
			if refreshable {
				old.OAuthCreds.RefreshToken = "old-refresh"
				replacement.OAuthCreds.RefreshToken = "new-refresh"
			}
			store := &credentialStore{current: old, replacement: replacement, changeAt: when}
			probe := cloneProvider(old)
			if _, err := ResolveToken(context.Background(), oauth.New(store), store, probe, false); err == nil {
				t.Errorf("account replacement at read %d was accepted: refreshable=%v", when, refreshable)
			}
			if probe.OAuthCreds.AccessToken != "old" || probe.OAuthCreds.ProfileArn != "arn:profile" {
				t.Error("failed resolution mutated credential snapshot")
			}
		}
	}
}

func TestResolveTokenPreservesSubmittedConfig(t *testing.T) {
	stored := credentialProvider("token")
	store := &credentialStore{current: stored}
	probe := cloneProvider(stored)
	probe.OAuthCreds.ProfileArn = "arn:submitted"
	probe.OAuthCreds.Region = "eu-central-1"
	token, err := ResolveToken(context.Background(), oauth.New(store), store, probe, false)
	if err != nil || token != "token" || probe.OAuthCreds.ProfileArn != "arn:submitted" || probe.OAuthCreds.Region != "eu-central-1" {
		t.Fatalf("resolution changed submitted config: token=%q err=%v", token, err)
	}
	if stored.OAuthCreds.ProfileArn != "arn:profile" {
		t.Fatal("resolution mutated stored profile")
	}
}

func TestResolveTokenRejectsConcurrentConfigEdit(t *testing.T) {
	stored := credentialProvider("token")
	replacement := cloneProvider(stored)
	replacement.OAuthCreds.ProfileArn = "arn:replacement"
	store := &credentialStore{current: stored, replacement: replacement, changeAt: 2}
	if _, err := ResolveToken(context.Background(), oauth.New(store), store, cloneProvider(stored), false); err == nil {
		t.Fatal("concurrent stored profile replacement was accepted")
	}
}
