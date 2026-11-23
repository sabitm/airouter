package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
)

const (
	grokBuildPollFloor     = time.Second
	grokBuildSlowDown      = 5 * time.Second
	grokBuildExpiryFloor   = 10 * time.Minute
	grokBuildAuthBodyLimit = 64 << 10
	grokBuildHTTPTimeout   = 15 * time.Second
	deviceGrantType        = "urn:ietf:params:oauth:grant-type:device_code"
)

// grokBuildWait waits for the device-poll interval or cancellation. Tests
// replace it. It is not a host override.
var grokBuildWait = func(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// GrokBuildConnect is one device-code login. The device code stays in this
// process. Callers may show only the verification URL and user code.
type GrokBuildConnect struct {
	state           string
	verificationURI string
	userCode        string
	deviceCode      string
	interval        time.Duration
	deadline        time.Time

	mu     sync.Mutex
	done   chan struct{}
	result exchangeResult
	cancel context.CancelFunc
}

// NewGrokBuildConnect requests a device code from the fixed issuer and returns
// a connector the dashboard can poll. It does not follow redirects.
func NewGrokBuildConnect(ctx context.Context) (*GrokBuildConnect, error) {
	state, err := newState()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, grokBuildHTTPTimeout)
	defer cancel()
	body, status, err := postGrokBuildForm(callCtx, grokbuild.DeviceCodeURL, url.Values{
		"client_id": {grokbuild.ClientID},
		"scope":     {grokbuild.Scopes},
		"referrer":  {grokbuild.Referrer},
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("grok-build: device authorization failed (HTTP %d)", status)
	}
	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.DeviceCode == "" || resp.UserCode == "" {
		return nil, errors.New("grok-build: device authorization returned an incomplete code")
	}
	if !validUserCode(resp.UserCode) {
		return nil, errors.New("grok-build: device authorization returned an invalid user code")
	}
	link := resp.VerificationURI
	if err := grokbuild.ValidateVerificationURI(link); err != nil {
		return nil, err
	}
	if resp.VerificationURIComplete != "" {
		if err := grokbuild.ValidateVerificationURI(resp.VerificationURIComplete); err != nil {
			return nil, err
		}
		link = resp.VerificationURIComplete
	}
	interval := time.Duration(resp.Interval) * time.Second
	if interval < grokBuildPollFloor {
		interval = 5 * time.Second
	}
	expires := time.Duration(resp.ExpiresIn) * time.Second
	if expires < grokBuildExpiryFloor {
		expires = grokBuildExpiryFloor
	}
	return &GrokBuildConnect{
		state:           state,
		verificationURI: link,
		userCode:        resp.UserCode,
		deviceCode:      resp.DeviceCode,
		interval:        interval,
		deadline:        time.Now().Add(expires),
		done:            make(chan struct{}),
	}, nil
}

func validUserCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, r := range code {
		if r > 127 || !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

func (d *GrokBuildConnect) State() string { return d.state }

func (d *GrokBuildConnect) VerificationURI() string { return d.verificationURI }

func (d *GrokBuildConnect) UserCode() string { return d.userCode }

// Start polls until approval, denial, expiry, or Close. The first poll waits
// for the server interval, matching the official client.
func (d *GrokBuildConnect) Start(context.Context) error {
	d.mu.Lock()
	if d.cancel != nil {
		d.mu.Unlock()
		return errors.New("grok-build: device connect already started")
	}
	pollCtx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.mu.Unlock()
	go d.poll(pollCtx)
	return nil
}

func (d *GrokBuildConnect) Result() (*domain.OAuthCreds, error, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return d.result.creds, d.result.err, true
	default:
		return nil, nil, false
	}
}

func (d *GrokBuildConnect) Close() error {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (d *GrokBuildConnect) finish(creds *domain.OAuthCreds, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return
	default:
	}
	d.result = exchangeResult{creds: creds, err: err}
	close(d.done)
}

func (d *GrokBuildConnect) poll(ctx context.Context) {
	interval := d.interval
	for {
		if !grokBuildWait(ctx, interval) {
			d.finish(nil, errors.New("grok-build: device sign-in cancelled"))
			return
		}
		if time.Now().After(d.deadline) {
			d.finish(nil, errors.New("grok-build: device code expired"))
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, grokBuildHTTPTimeout)
		body, status, err := postGrokBuildForm(callCtx, grokbuild.TokenURL, url.Values{
			"grant_type":  {deviceGrantType},
			"device_code": {d.deviceCode},
			"client_id":   {grokbuild.ClientID},
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				d.finish(nil, errors.New("grok-build: device sign-in cancelled"))
				return
			}
			d.finish(nil, errors.New("grok-build: device token request failed"))
			return
		}
		creds, action, err := classifyDeviceToken(body, status, time.Now())
		switch action {
		case deviceSlowDown:
			interval += grokBuildSlowDown
		case deviceDone:
			d.finish(creds, err)
			return
		}
	}
}

type devicePollAction int

const (
	deviceContinue devicePollAction = iota
	deviceSlowDown
	deviceDone
)

func classifyDeviceToken(body []byte, status int, now time.Time) (*domain.OAuthCreds, devicePollAction, error) {
	var tr struct {
		AccessToken      string          `json:"access_token"`
		RefreshToken     string          `json:"refresh_token"`
		ExpiresIn        int             `json:"expires_in"`
		Scope            string          `json:"scope"`
		IDToken          string          `json:"id_token"`
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if json.Unmarshal(body, &tr) != nil {
		if status >= 200 && status < 300 {
			return nil, deviceDone, errors.New("grok-build: device token response was not valid JSON")
		}
		return nil, deviceDone, fmt.Errorf("grok-build: device token request failed (HTTP %d)", status)
	}
	code, _ := parseTokenError(tr.Error)
	if code == "" && status >= 200 && status < 300 && tr.AccessToken != "" {
		if tr.RefreshToken == "" {
			return nil, deviceDone, errors.New("grok-build: device token response omitted a refresh token")
		}
		creds, err := grokBuildCreds(tr.AccessToken, tr.RefreshToken, tr.Scope, tr.IDToken, tr.ExpiresIn, now)
		if err != nil {
			return nil, deviceDone, err
		}
		return creds, deviceDone, nil
	}
	switch code {
	case "authorization_pending":
		return nil, deviceContinue, nil
	case "slow_down":
		return nil, deviceSlowDown, nil
	case "access_denied":
		return nil, deviceDone, errors.New("grok-build: authorization denied")
	case "expired_token":
		return nil, deviceDone, errors.New("grok-build: device code expired")
	case "":
		return nil, deviceDone, fmt.Errorf("grok-build: device token request failed (HTTP %d)", status)
	default:
		return nil, deviceDone, errors.New("grok-build: device token request failed")
	}
}

func newGrokBuildConnection() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("grok-build: could not mint a connection id")
	}
	return hex.EncodeToString(b[:]), nil
}

func grokBuildCreds(access, refresh, scope, idToken string, expiresIn int, now time.Time) (*domain.OAuthCreds, error) {
	connection, err := newGrokBuildConnection()
	if err != nil {
		return nil, err
	}
	if scope == "" {
		scope = grokbuild.Scopes
	}
	if scope != grokbuild.Scopes {
		// Keep the requested set. A granted echo that differs is not stored as
		// a new client configuration.
		scope = grokbuild.Scopes
	}
	userID, email := grokBuildIdentity(access, idToken)
	creds := &domain.OAuthCreds{
		Mode:                domain.OAuthAuto,
		Preset:              grokbuild.Preset,
		AccessToken:         access,
		RefreshToken:        refresh,
		Scopes:              scope,
		AuthURL:             grokbuild.Issuer,
		TokenURL:            grokbuild.TokenURL,
		ClientID:            grokbuild.ClientID,
		GrokBuildAuth:       true,
		GrokBuildConnection: connection,
	}
	if expiresIn > 0 {
		creds.ExpiresAt = now.Add(time.Duration(expiresIn) * time.Second).Unix()
	}
	creds.UserID = userID
	creds.Email = email
	return creds, nil
}

// grokBuildIdentity reads the catalog user id the CLI sends as x-userid.
// A team or organization principal replaces the id-token subject. Email is
// display metadata and is not sent upstream.
func grokBuildIdentity(access, idToken string) (userID, email string) {
	claims := unverifiedJWTClaims(access)
	principalType := claims.PrincipalType
	if principalType == "" {
		principalType = claims.PrincipalTypeCamel
	}
	principalID := claims.PrincipalID
	if principalID == "" {
		principalID = claims.PrincipalIDCamel
	}
	switch principalType {
	case "Team", "Organization":
		if principalID != "" {
			return principalID, ""
		}
	}
	idClaims := unverifiedJWTClaims(idToken)
	if idClaims.Email != "" {
		email = idClaims.Email
	}
	if idClaims.Subject != "" {
		userID = idClaims.Subject
	}
	return userID, email
}

type grokJWTClaims struct {
	Subject            string `json:"sub"`
	Email              string `json:"email"`
	PrincipalType      string `json:"principal_type"`
	PrincipalID        string `json:"principal_id"`
	PrincipalIDCamel   string `json:"principalId"`
	PrincipalTypeCamel string `json:"principalType"`
}

func unverifiedJWTClaims(token string) grokJWTClaims {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return grokJWTClaims{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return grokJWTClaims{}
	}
	var claims grokJWTClaims
	if json.Unmarshal(raw, &claims) != nil {
		return grokJWTClaims{}
	}
	return claims
}

func postGrokBuildForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	if !grokbuild.IsFixedDeviceURL(endpoint) && !grokbuild.IsFixedTokenURL(endpoint) {
		return nil, 0, errors.New("grok-build: token request host is not fixed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", grokbuild.UserAgent())
	req.Header.Set("X-Grok-Client-Version", grokbuild.Version)
	req.Header.Set("X-Grok-Client-Surface", grokbuild.ClientSurface)
	resp, err := grokbuild.DoFixed(ctx, req)
	if err != nil {
		return nil, 0, errors.New("grok-build: token request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, grokBuildAuthBodyLimit+1))
	if err != nil || len(body) > grokBuildAuthBodyLimit {
		return nil, resp.StatusCode, errors.New("grok-build: token response was too large")
	}
	return body, resp.StatusCode, nil
}

func refreshGrokBuild(ctx context.Context, c *domain.OAuthCreds, now time.Time) error {
	if c == nil || !c.GrokBuildAuth {
		return errors.New("grok-build: credentials are not valid for this provider")
	}
	if c.ClientID != "" && c.ClientID != grokbuild.ClientID {
		return errors.New("grok-build: client id is not the subscription client")
	}
	if c.TokenURL != "" && !grokbuild.IsFixedTokenURL(c.TokenURL) {
		return errors.New("grok-build: token URL is not the official endpoint")
	}
	if c.RefreshToken == "" {
		return ErrInvalidGrant
	}
	callCtx, cancel := context.WithTimeout(ctx, grokBuildHTTPTimeout)
	defer cancel()
	body, status, err := postGrokBuildForm(callCtx, grokbuild.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {grokbuild.ClientID},
	})
	if err != nil {
		return err
	}
	var tr tokenResponse
	if json.Unmarshal(body, &tr) != nil {
		if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden {
			return ErrInvalidGrant
		}
		return fmt.Errorf("grok-build: refresh failed (HTTP %d)", status)
	}
	code, _ := parseTokenError(tr.Error)
	if code != "" {
		if isInvalidGrantCode(code) {
			return ErrInvalidGrant
		}
		return errors.New("grok-build: refresh failed")
	}
	if status < 200 || status >= 300 || tr.AccessToken == "" {
		if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden {
			return ErrInvalidGrant
		}
		return fmt.Errorf("grok-build: refresh failed (HTTP %d)", status)
	}
	// GrokBuildConnection is not a token field. Leave the login generation in
	// place so a refresh cannot look like a reconnect of the same account.
	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	userID, email := grokBuildIdentity(tr.AccessToken, tr.IDToken)
	if userID != "" {
		c.UserID = userID
	}
	if email != "" {
		c.Email = email
	}
	c.IDToken = ""
	c.GrokBuildAuth = true
	c.Preset = grokbuild.Preset
	c.ClientID = grokbuild.ClientID
	c.TokenURL = grokbuild.TokenURL
	c.AuthURL = grokbuild.Issuer
	c.Scopes = grokbuild.Scopes
	return nil
}

func grokBuildCredsOf(p *domain.Provider) *domain.OAuthCreds {
	if p == nil {
		return nil
	}
	return p.OAuthCreds
}

func rejectGrokBuildMismatch(p *domain.Provider, creds *domain.OAuthCreds) error {
	marked := grokbuild.Marked(creds)
	proto := p != nil && p.Protocol == domain.ProtocolGrokBuild
	if !marked && !proto {
		return nil
	}
	if p == nil || !proto || !marked {
		return errors.New("grok-build: credentials are not valid for this provider")
	}
	cp := *p
	cp.OAuthCreds = creds
	return grokbuild.ValidateCredentialUse(&cp)
}
