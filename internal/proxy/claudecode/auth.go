package claudecode

import (
	"strings"

	"airouter/internal/domain"
)

// inferenceScope is the exact grant item that selects the oauth beta.
// A different case, prefix, suffix, or adjacent punctuation is not this item.
const inferenceScope = "user:inference"

// OAuthBeta reports whether this request-local provider should receive the
// oauth beta. It classifies only that beta. It does not authorize inference,
// change the auth method or scheme, expand grants, or rewrite credentials.
// Method gates stored grants. Scheme and protocol are not inputs.
//
// An empty effective token is false. For an OAuth-method provider, a nonempty
// ClaudeScopes list is authoritative only when OAuthCreds.AccessToken equals
// provider.APIKey. The list is split on whitespace. The exact user:inference
// item is required. A known grant without that item is false even when the
// token contains the legacy OAuth marker.
//
// An unknown grant, including a whitespace-only list, uses IsOAuthToken.
// That fallback includes an imported static bearer token. An API-key provider
// ignores config-only OAuthCreds scopes. A token that does not match the
// credential snapshot never uses those scopes.
func OAuthBeta(p *domain.Provider) bool {
	if p == nil || p.APIKey == "" {
		return false
	}
	if p.Method() == domain.AuthOAuth && p.OAuthCreds != nil && knownClaudeGrant(p.OAuthCreds.ClaudeScopes) && p.OAuthCreds.AccessToken == p.APIKey {
		return grantHasInference(p.OAuthCreds.ClaudeScopes)
	}
	return IsOAuthToken(p.APIKey)
}

// knownClaudeGrant is false for an empty or whitespace-only scope list.
// Those values mean the grant is unknown, not that inference was denied.
func knownClaudeGrant(scopes string) bool {
	return strings.TrimSpace(scopes) != ""
}

func grantHasInference(scopes string) bool {
	for _, item := range strings.Fields(scopes) {
		if item == inferenceScope {
			return true
		}
	}
	return false
}
