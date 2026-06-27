package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
)

// Connect drives a single OAuth authorization-code (+ PKCE for public clients)
// flow from start to token exchange. It is created per connection attempt and
// holds the session state (verifier, state) that ties the authorize redirect to
// the token exchange. Two completion paths share one Connect:
//
//   - Loopback: Start binds a local server on the redirect_uri port and serves
//     the callback; the browser's redirect is exchanged automatically.
//   - Manual paste: the operator copies the full redirect URL (or just the code)
//     and calls ExchangeCode, for hosts where the browser cannot reach airouter.
type Connect struct {
	creds    *domain.OAuthCreds // config; tokens filled on success
	verifier string
	state    string

	mu      sync.Mutex
	srv     *http.Server
	addr    string        // actual bound address (host:port) once Start succeeds
	done    chan struct{} // closed when the flow completes (callback or paste)
	result  exchangeResult
	started bool

	// baseURL overrides the token/auth URLs during tests; empty in production.
	baseURL string
}

// Addr returns the loopback server's bound address (host:port) after Start, or
// "" if Start was not called. Useful when the redirect_uri specified port 0 and
// the OS chose the actual port.
func (c *Connect) Addr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addr
}

// State returns the opaque state token tying this attempt's authorize redirect
// to its token exchange. The dashboard uses it as the connect-session key.
func (c *Connect) State() string { return c.state }

// Result reports the flow outcome without blocking, for a status poll. done is
// false while the flow is still in progress (no callback or paste yet); when
// true, creds is the connected credentials or err describes the failure. Unlike
// Wait it never blocks, so a polling handler can return immediately.
func (c *Connect) Result() (creds *domain.OAuthCreds, err error, done bool) {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.result.creds, c.result.err, true
	default:
		return nil, nil, false
	}
}

type exchangeResult struct {
	creds *domain.OAuthCreds
	err   error
}

// NewConnect prepares an authorization-code flow for the given OAuth config. The
// tokens are populated by AuthorizeURL -> (callback or ExchangeCode) -> wait.
func NewConnect(creds *domain.OAuthCreds) (*Connect, error) {
	if creds == nil {
		return nil, errors.New("oauth: nil creds")
	}
	if creds.AuthURL == "" || creds.TokenURL == "" || creds.ClientID == "" {
		return nil, errors.New("oauth: connect requires auth_url, token_url, client_id")
	}
	verifier, err := newVerifier()
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, err
	}
	return &Connect{creds: creds, verifier: verifier, state: state, done: make(chan struct{})}, nil
}

// AuthorizeURL builds the authorization endpoint URL the user's browser visits.
// It includes the PKCE code_challenge for public clients (PKCE=true).
func (c *Connect) AuthorizeURL() (string, error) {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.creds.ClientID)
	q.Set("redirect_uri", c.creds.RedirectURI)
	q.Set("state", c.state)
	q.Set("scope", c.creds.Scopes)
	if c.creds.PKCE {
		q.Set("code_challenge", challengeS256(c.verifier))
		q.Set("code_challenge_method", "S256")
	}
	authURL := c.creds.AuthURL
	if c.baseURL != "" {
		authURL = c.baseURL + "/authorize"
	}
	return authURL + "?" + q.Encode(), nil
}

// Start binds a loopback server on the redirect_uri port to receive the callback
// and auto-exchange the code, then serves it in the background. It returns once
// the port is bound (so a bind failure surfaces synchronously); Wait returns the
// flow outcome. Calling Start is optional: a remote operator can skip it and use
// ExchangeCode with a pasted redirect URL instead.
func (c *Connect) Start(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("oauth: connect already started")
	}
	port, err := loopbackPort(c.creds.RedirectURI)
	if err != nil {
		return err
	}
	// Bind synchronously so a port conflict (another connect in flight) is
	// reported to the caller rather than swallowed by the background goroutine.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("oauth: bind callback server on :%d: %w", port, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", c.handleCallback)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	c.srv = srv
	c.addr = ln.Addr().String()
	c.started = true
	go func() {
		_ = srv.Serve(ln) // returns ErrServerClosed on Close; nothing to do
	}()
	return nil
}

// handleCallback validates state, exchanges the code, and signals Wait.
func (c *Connect) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("error") != "" {
		c.finish(w, http.StatusBadRequest, "OAuth error: "+q.Get("error"))
		return
	}
	if q.Get("state") != c.state {
		c.finish(w, http.StatusBadRequest, "state mismatch")
		return
	}
	code := q.Get("code")
	if code == "" {
		c.finish(w, http.StatusBadRequest, "missing code")
		return
	}
	if _, err := c.exchange(r.Context(), code); err != nil {
		c.finish(w, http.StatusBadGateway, "token exchange failed: "+err.Error())
		return
	}
	c.finish(w, http.StatusOK, "auth complete - you may close this tab")
}

// finish replies to the browser and, for the early error paths (bad state,
// missing code) that never reached exchange(), records the failure so Wait
// unblocks. On the success path exchange() has already set c.result and closed
// done, so the guard below is a no-op.
func (c *Connect) finish(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.result.creds == nil && c.result.err == nil {
		c.result = exchangeResult{err: fmt.Errorf("oauth: %s", msg)}
		close(c.done)
	}
}

// ExchangeCode completes the flow from a pasted authorization code or full
// redirect URL. Used when the browser cannot reach airouter's loopback server
// (remote host). It is also the path the loopback callback uses internally.
func (c *Connect) ExchangeCode(ctx context.Context, codeOrURL string) (*domain.OAuthCreds, error) {
	code := codeOrURL
	if strings.HasPrefix(codeOrURL, "http://") || strings.HasPrefix(codeOrURL, "https://") {
		u, err := url.Parse(codeOrURL)
		if err != nil {
			return nil, fmt.Errorf("oauth: parse pasted URL: %w", err)
		}
		if s := u.Query().Get("state"); s != "" && s != c.state {
			return nil, errors.New("oauth: state mismatch")
		}
		code = u.Query().Get("code")
	}
	if code == "" {
		return nil, errors.New("oauth: no code in pasted input")
	}
	return c.exchange(ctx, code)
}

// exchange performs the token request once, memoizing the result so the loopback
// callback and a concurrent manual paste do not double-exchange.
func (c *Connect) exchange(ctx context.Context, code string) (*domain.OAuthCreds, error) {
	c.mu.Lock()
	if c.result.creds != nil || c.result.err != nil {
		creds, err := c.result.creds, c.result.err
		c.mu.Unlock()
		return creds, err
	}
	c.mu.Unlock()

	cp := *c.creds
	if err := exchangeCode(ctx, &cp, code, c.verifier, c.baseURL); err != nil {
		c.mu.Lock()
		c.result = exchangeResult{err: err}
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	c.creds = &cp
	c.result = exchangeResult{creds: &cp}
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	c.mu.Unlock()
	return &cp, nil
}

// Wait blocks until the flow completes (loopback callback or manual exchange)
// or ctx is canceled, returning the resulting creds. Call after Start and/or
// after the user has been sent to AuthorizeURL.
func (c *Connect) Wait(ctx context.Context) (*domain.OAuthCreds, error) {
	select {
	case <-c.done:
		return c.result.creds, c.result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close releases the loopback server if Start was called. Safe to call once;
// idempotent.
func (c *Connect) Close() error {
	c.mu.Lock()
	srv := c.srv
	c.srv = nil
	c.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// exchangeCode posts the authorization-code grant, populating creds with the
// response. baseURL overrides the token URL in tests (empty in production).
func exchangeCode(ctx context.Context, c *domain.OAuthCreds, code, verifier, baseURL string) error {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", c.ClientID)
	form.Set("redirect_uri", c.RedirectURI)
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	if c.PKCE {
		form.Set("code_verifier", verifier)
	}

	tokenURL := c.TokenURL
	if baseURL != "" {
		tokenURL = baseURL + "/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: exchange request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := readLimited(resp.Body)

	var tr tokenResponse
	if err := jsonUnmarshal(body, &tr); err != nil {
		return fmt.Errorf("oauth: exchange: decode %d: %w", resp.StatusCode, err)
	}
	if tr.Error != "" {
		return fmt.Errorf("oauth: exchange: %s: %s", tr.Error, tr.ErrorDescription)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("oauth: exchange: empty access_token (HTTP %d)", resp.StatusCode)
	}

	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		c.IDToken = tr.IDToken
		if email, ok := emailFromIDToken(tr.IDToken); ok {
			c.Email = email
		}
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	return nil
}
