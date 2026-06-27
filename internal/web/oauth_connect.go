package web

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
)

// credsFromConnectForm builds an OAuthCreds config (no tokens yet) from the
// connect form. A chosen preset supplies the whole configuration; otherwise the
// manual fields are read verbatim, making the flow work for any provider.
func credsFromConnectForm(r *http.Request) (*domain.OAuthCreds, error) {
	if name := r.FormValue("preset"); name != "" && name != "custom" {
		p, ok := oauth.PresetByName(name)
		if !ok {
			return nil, fieldError("unknown preset")
		}
		_, creds := oauth.Apply(p)
		return creds, nil
	}
	return &domain.OAuthCreds{
		Mode:         domain.OAuthManual,
		AuthURL:      strings.TrimSpace(r.FormValue("auth_url")),
		TokenURL:     strings.TrimSpace(r.FormValue("token_url")),
		ClientID:     strings.TrimSpace(r.FormValue("client_id")),
		ClientSecret: strings.TrimSpace(r.FormValue("client_secret")),
		Scopes:       strings.TrimSpace(r.FormValue("scopes")),
		RedirectURI:  strings.TrimSpace(r.FormValue("redirect_uri")),
		PKCE:         r.FormValue("pkce") == "on" || r.FormValue("pkce") == "true",
	}, nil
}

type fieldError string

func (e fieldError) Error() string { return string(e) }

// applyManualTokens overlays user-pasted tokens onto a config-only creds, for
// importing an already-authenticated session (e.g. from a CLI) without running
// the browser connect flow. It returns false when neither token is present, so
// the caller can fall back to the connect-session path. The config (token_url,
// client_id, pkce) still comes from credsFromConnectForm; only the refresh token
// is strictly required for the token to outlive its expiry.
func applyManualTokens(c *domain.OAuthCreds, r *http.Request) bool {
	access := strings.TrimSpace(r.FormValue("access_token"))
	refresh := strings.TrimSpace(r.FormValue("refresh_token"))
	if access == "" && refresh == "" {
		return false
	}
	c.AccessToken = access
	c.RefreshToken = refresh
	c.Email = strings.TrimSpace(r.FormValue("email"))
	c.ExpiresAt = parseExpiresAt(r.FormValue("expires_at"))
	return true
}

// parseExpiresAt reads an access-token expiry as either a unix-seconds integer or
// an RFC3339 timestamp (the shape CLIs commonly emit). An empty or unparseable
// value yields 0 (unknown expiry), which leaves refreshing to the reactive 401
// path rather than the proactive one.
func parseExpiresAt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}

// connectPhase reads a session's current outcome without blocking, mapping it to
// the (phase, email, errMsg) triple the status line renders. Phases: "pending"
// (no callback or paste yet), "connected" (tokens obtained), "error".
func connectPhase(sess *connectSession) (phase, email, errMsg string) {
	creds, err, done := sess.conn.Result()
	switch {
	case !done:
		return "pending", "", ""
	case err != nil:
		return "error", "", err.Error()
	default:
		return "connected", creds.Email, ""
	}
}

// beginOAuthConnect starts an authorization-code flow: it builds the connect
// configuration from the form (preset or manual), binds the loopback callback
// server when the redirect URI allows it, and renders the connect region (the
// authorize link, a manual-paste fallback, and a polling status line). A
// loopback bind failure is non-fatal: the manual-paste path still completes the
// flow on hosts whose browser cannot reach the loopback server.
func (h *Handler) beginOAuthConnect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, OAuthConnectError("invalid form"))
		return
	}
	creds, err := credsFromConnectForm(r)
	if err != nil {
		render(w, r, OAuthConnectError(err.Error()))
		return
	}
	conn, err := oauth.NewConnect(creds)
	if err != nil {
		render(w, r, OAuthConnectError(err.Error()))
		return
	}
	if err := conn.Start(r.Context()); err != nil && h.trace {
		log.Printf("[trace] oauth connect loopback bind failed: %v", err)
	}
	authURL, err := conn.AuthorizeURL()
	if err != nil {
		conn.Close()
		render(w, r, OAuthConnectError(err.Error()))
		return
	}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	// A bound loopback address means the callback can complete the flow in the
	// background, so the status line polls for it; without it only the pasted code
	// drives completion.
	render(w, r, OAuthConnectView(authURL, conn.State(), conn.Addr() != ""))
}

// oauthConnectStatus re-renders only the status line for the 2s poll. It never
// blocks; a completed flow renders a terminal line that omits the poll trigger,
// stopping the poll.
func (h *Handler) oauthConnectStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	sess, ok := h.sessions.get(state)
	if !ok {
		render(w, r, OAuthStatusLine(state, false, "error", "", "connect session expired - start over"))
		return
	}
	phase, email, errMsg := connectPhase(sess)
	render(w, r, OAuthStatusLine(state, sess.conn.Addr() != "", phase, email, errMsg))
}

// oauthConnectExchange completes the flow from a pasted authorization code or
// full redirect URL, for hosts where the loopback callback is unreachable. It
// renders the resulting status line.
func (h *Handler) oauthConnectExchange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, OAuthStatusLine("", false, "error", "", "invalid form"))
		return
	}
	state := r.FormValue("state")
	sess, ok := h.sessions.get(state)
	if !ok {
		render(w, r, OAuthStatusLine(state, false, "error", "", "connect session expired - start over"))
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	if code == "" {
		render(w, r, OAuthStatusLine(state, sess.conn.Addr() != "", "error", "", "paste the code or full redirect URL"))
		return
	}
	if _, err := sess.conn.ExchangeCode(r.Context(), code); err != nil {
		render(w, r, OAuthStatusLine(state, sess.conn.Addr() != "", "error", "", err.Error()))
		return
	}
	phase, email, errMsg := connectPhase(sess)
	render(w, r, OAuthStatusLine(state, sess.conn.Addr() != "", phase, email, errMsg))
}

// oauthConnectCancel abandons an in-flight connect, releasing its loopback
// server, and clears the connect region.
func (h *Handler) oauthConnectCancel(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	h.sessions.drop(r.FormValue("state"))
	render(w, r, OAuthConnectIdle())
}

// --- template helpers ---

// boolStr renders a bool as the literal "true"/"false" for a data attribute.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// emailSuffix renders the " as <email>" tail of a connected status line, empty
// when the provider did not return an email (no id_token / email scope).
func emailSuffix(email string) string {
	if email == "" {
		return ""
	}
	return " as " + email
}

// oauthLabel is the credential-column text for a connected oauth provider: the
// account email when known, otherwise a generic "connected".
func oauthLabel(c *domain.OAuthCreds) string {
	if c != nil && c.Email != "" {
		return c.Email
	}
	return "connected"
}

// defaultRedirectURI is the loopback callback the connect flow binds when the
// user has not supplied one. It matches the xAI preset so the common case needs
// no input.
const defaultRedirectURI = "http://127.0.0.1:56121/callback"

// orEmptyCreds returns a non-nil creds to read defaults from in the edit form,
// so the template can prefill manual fields without nil checks.
func orEmptyCreds(c *domain.OAuthCreds) *domain.OAuthCreds {
	if c != nil {
		return c
	}
	return &domain.OAuthCreds{}
}

// redirectOr defaults a blank redirect URI to the loopback callback.
func redirectOr(s string) string {
	if s == "" {
		return defaultRedirectURI
	}
	return s
}
