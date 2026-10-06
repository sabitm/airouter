package kirocatalog

import (
	"errors"
	"fmt"
	"net/http"

	"airouter/internal/oauth"
)

var (
	ErrInvalidBaseURL = errors.New("invalid Kiro base URL")
	ErrTruncated      = errors.New("Kiro catalog response truncated")
	ErrShape          = errors.New("Kiro catalog response shape unexpected")
	ErrEmpty          = errors.New("Kiro catalog is empty")
	ErrPageLimit      = errors.New("Kiro catalog page limit exceeded")
	ErrTokenRepeat    = errors.New("Kiro catalog nextToken repeated")
	ErrUnavailable    = errors.New("Kiro catalog unavailable")
)

// RefreshError wraps a failed catalog token refresh. It is not an auth status.
type RefreshError struct {
	Err error
}

func (e *RefreshError) Error() string {
	return "Kiro catalog token refresh failed"
}

func (e *RefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// StatusError is a received non-2xx catalog response. Status wins over a later
// body-read error.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string {
	if e == nil {
		return "HTTP 0"
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// TransportError is a failure to reach the catalog or to read a response body
// when no authoritative status or truncation was received.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string {
	return "catalog transport failed"
}

func (e *TransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AuthStatus returns 401 or 403 when err is that catalog status. Other errors
// return 0.
func AuthStatus(err error) int {
	var status *StatusError
	if errors.As(err, &status) && (status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden) {
		return status.Status
	}
	return 0
}

// DashboardText maps a catalog error to the existing dashboard phrase. Unknown
// errors stay reachable-failure text. It never includes response bodies.
func DashboardText(err error) string {
	switch {
	case errors.Is(err, ErrInvalidBaseURL):
		return "invalid base URL"
	case errors.Is(err, ErrTruncated):
		return "catalog response truncated"
	case errors.Is(err, ErrShape):
		return "catalog response shape unexpected"
	case errors.Is(err, ErrEmpty):
		return "catalog is empty"
	case errors.Is(err, ErrPageLimit):
		return "catalog page limit exceeded"
	case errors.Is(err, ErrTokenRepeat):
		return "catalog pagination repeated nextToken"
	case errors.Is(err, contextCanceled()):
		return "catalog request canceled"
	case errors.Is(err, contextDeadline()):
		return "catalog request timed out"
	}
	var refresh *RefreshError
	if errors.As(err, &refresh) {
		return refreshText(refresh.Err)
	}
	var status *StatusError
	if errors.As(err, &status) {
		switch status.Status {
		case http.StatusUnauthorized:
			return "credential rejected (HTTP 401)"
		case http.StatusForbidden:
			return "access denied (HTTP 403)"
		case http.StatusNotFound:
			return "not found (HTTP 404) - check base URL"
		default:
			if status.Status >= 500 {
				return fmt.Sprintf("upstream unavailable (HTTP %d)", status.Status)
			}
			return fmt.Sprintf("upstream returned HTTP %d", status.Status)
		}
	}
	return "could not reach Kiro"
}

func refreshText(err error) string {
	if errors.Is(err, oauth.ErrInvalidGrant) {
		return "token expired - reconnect required"
	}
	return "token refresh failed"
}
