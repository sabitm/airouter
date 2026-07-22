package oauth

import (
	"context"
	"time"

	"airouter/internal/domain"
)

// refreshCursor always fails: Cursor IDE access tokens are short-lived sessions
// imported from the IDE's local state, with no refresh endpoint. The reactive
// 401 path surfaces this as ErrInvalidGrant so the user is prompted to re-paste
// a fresh token. Mirrors refreshQoder for device/imported tokens.
func refreshCursor(_ context.Context, _ *domain.OAuthCreds, _ time.Time) error {
	return ErrInvalidGrant
}
