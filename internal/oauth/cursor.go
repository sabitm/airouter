package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
)

var cursorURLConfig = struct {
	sync.RWMutex
	login    string
	poll     string
	exchange string
}{
	login:    "https://cursor.com/loginDeepControl",
	poll:     "https://api2.cursor.sh/auth/poll",
	exchange: "https://api2.cursor.sh/auth/exchange_user_api_key",
}

// OverrideCursorURLs points login/poll/exchange at test hosts. Empty values keep
// the current URL. The returned restore function puts the previous values back.
func OverrideCursorURLs(login, poll, exchange string) func() {
	cursorURLConfig.Lock()
	origLogin, origPoll, origExchange := cursorURLConfig.login, cursorURLConfig.poll, cursorURLConfig.exchange
	if login != "" {
		cursorURLConfig.login = login
	}
	if poll != "" {
		cursorURLConfig.poll = poll
	}
	if exchange != "" {
		cursorURLConfig.exchange = exchange
	}
	cursorURLConfig.Unlock()
	return func() {
		cursorURLConfig.Lock()
		cursorURLConfig.login, cursorURLConfig.poll, cursorURLConfig.exchange = origLogin, origPoll, origExchange
		cursorURLConfig.Unlock()
	}
}

func cursorURLs() (login, poll, exchange string) {
	cursorURLConfig.RLock()
	defer cursorURLConfig.RUnlock()
	return cursorURLConfig.login, cursorURLConfig.poll, cursorURLConfig.exchange
}

const (
	cursorConnectTTL     = 10 * time.Minute
	cursorPollInitialDef = time.Second
	cursorPollMaxDef     = 10 * time.Second
)

// Poll timing is overridable so tests can run without sleeping on production intervals.
var (
	cursorPollInitial = cursorPollInitialDef
	cursorPollMax     = cursorPollMaxDef
	cursorLoginTTL    = cursorConnectTTL
)

// CursorConnect drives Cursor's browser-and-poll login (PKCE + uuid, no loopback).
type CursorConnect struct {
	state     string
	verifier  string
	authURL   string
	pollURL   string
	machineID string

	pollInitial time.Duration
	pollMax     time.Duration
	loginTTL    time.Duration

	mu      sync.Mutex
	done    chan struct{}
	stopped chan struct{}
	result  exchangeResult
	started bool
	cancel  context.CancelFunc
}

// NewCursorConnect prepares a Cursor login attempt. existingMachineID is reused
// on reconnect so the checksum identity stays stable; a new connect generates one.
func NewCursorConnect(existingMachineID string) (*CursorConnect, error) {
	loginUUID, err := newUUIDString()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := cursorPKCE()
	if err != nil {
		return nil, err
	}
	machineID := strings.TrimSpace(existingMachineID)
	if machineID == "" {
		machineID, err = newCursorMachineID()
		if err != nil {
			return nil, err
		}
	}
	loginURL, pollURL, _ := cursorURLs()
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("uuid", loginUUID)
	q.Set("mode", "login")
	q.Set("redirectTarget", "cli")
	return &CursorConnect{
		state:       loginUUID,
		verifier:    verifier,
		authURL:     loginURL + "?" + q.Encode(),
		pollURL:     pollURL,
		machineID:   machineID,
		pollInitial: cursorPollInitial,
		pollMax:     cursorPollMax,
		loginTTL:    cursorLoginTTL,
		done:        make(chan struct{}),
		stopped:     make(chan struct{}),
	}, nil
}

func (d *CursorConnect) State() string { return d.state }

func (d *CursorConnect) MachineID() string { return d.machineID }

func (d *CursorConnect) LoginURL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.authURL
}

// Start begins background polling. The user opens LoginURL in a browser.
func (d *CursorConnect) Start(_ context.Context) error {
	pollCtx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		cancel()
		return errors.New("oauth: cursor connect already started")
	}
	d.started = true
	d.cancel = cancel
	go func() {
		defer close(d.stopped)
		d.pollLoop(pollCtx)
	}()
	d.mu.Unlock()
	return nil
}

func (d *CursorConnect) Result() (*domain.OAuthCreds, error, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return d.result.creds, d.result.err, true
	default:
		return nil, nil, false
	}
}

func (d *CursorConnect) Close() error {
	d.mu.Lock()
	cancel := d.cancel
	started := d.started
	stopped := d.stopped
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if started {
		<-stopped
	}
	return nil
}

func (d *CursorConnect) pollLoop(ctx context.Context) {
	deadline := time.Now().Add(d.loginTTL)
	wait := d.pollInitial
	if wait <= 0 {
		wait = cursorPollInitialDef
	}

	if d.pollOnce(ctx) {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			d.finish(nil, ctx.Err())
			return
		case <-timer.C:
			if time.Now().After(deadline) {
				d.finish(nil, errors.New("oauth: cursor authorization timed out"))
				return
			}
			if d.pollOnce(ctx) {
				return
			}
			wait *= 6
			wait /= 5
			if wait > d.pollMax && d.pollMax > 0 {
				wait = d.pollMax
			}
			timer.Reset(wait)
		}
	}
}

// pollOnce returns true when the flow completed (success or terminal error).
func (d *CursorConnect) pollOnce(ctx context.Context) bool {
	q := url.Values{}
	q.Set("uuid", d.state)
	q.Set("verifier", d.verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.pollURL+"?"+q.Encode(), nil)
	if err != nil {
		d.finish(nil, err)
		return true
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			d.finish(nil, ctx.Err())
			return true
		}
		return false
	}
	defer resp.Body.Close()
	body, _ := readLimited(resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return false
	case resp.StatusCode != http.StatusOK:
		d.finish(nil, fmt.Errorf("oauth: cursor poll failed: HTTP %d", resp.StatusCode))
		return true
	}

	var payload cursorTokenPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		d.finish(nil, fmt.Errorf("oauth: cursor poll: invalid JSON: %w", err))
		return true
	}
	if payload.AccessToken == "" {
		d.finish(nil, errors.New("oauth: cursor poll returned 200 but no accessToken"))
		return true
	}
	if payload.RefreshToken == "" {
		d.finish(nil, errors.New("oauth: cursor poll returned 200 but no refreshToken"))
		return true
	}
	d.finish(d.credsFromTokens(payload), nil)
	return true
}

func (d *CursorConnect) credsFromTokens(payload cursorTokenPayload) *domain.OAuthCreds {
	creds := &domain.OAuthCreds{
		Mode:         domain.OAuthAuto,
		Preset:       "cursor",
		CursorAuth:   true,
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		MachineID:    d.machineID,
		ExpiresAt:    CursorTokenExpiry(payload.AccessToken),
	}
	if email, accountID, ok := ClaimsFromToken(payload.AccessToken); ok {
		creds.Email = email
		creds.AccountID = accountID
	}
	return creds
}

func (d *CursorConnect) finish(creds *domain.OAuthCreds, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return
	default:
		d.result = exchangeResult{creds: creds, err: err}
		close(d.done)
	}
}

type cursorTokenPayload struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// ErrCursorNotRotatable means Cursor rejected exchange (or the stored secret is
// a browser session JWT) while the access token is still usable. Dashboard
// refresh must not say reconnect; the next chat can keep using AccessToken.
var ErrCursorNotRotatable = errors.New("oauth: cursor session cannot be rotated")

const cursorSessionIssuer = "https://authentication.cursor.sh"

// cursorSessionJWT reports whether token is a Cursor browser/CLI session JWT.
// Those cannot be sent to exchange_user_api_key (401 Invalid User API Key).
func cursorSessionJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false
	}
	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return false
	}
	return claims.Iss == cursorSessionIssuer
}

// cursorUserAPIKey reports whether token looks like the durable secret
// exchange_user_api_key accepts. Empty and Cursor session JWTs do not.
func cursorUserAPIKey(token string) bool {
	token = strings.TrimSpace(token)
	return token != "" && !cursorSessionJWT(token)
}

func cursorAccessUsable(c *domain.OAuthCreds, now time.Time) bool {
	if c == nil {
		return false
	}
	exp := c.ExpiresAt
	if exp == 0 {
		exp = CursorTokenExpiry(c.AccessToken)
	}
	if exp == 0 {
		return false
	}
	return now.Before(time.Unix(exp, 0))
}

// refreshCursor exchanges a Cursor user API key for a new access token.
// Browser poll tokens are session JWTs and must not hit exchange. Access-only
// imports and expired sessions surface ErrInvalidGrant so the 401 path
// prompts reconnect.
func refreshCursor(ctx context.Context, c *domain.OAuthCreds, now time.Time) error {
	if c == nil || strings.TrimSpace(c.RefreshToken) == "" {
		return ErrInvalidGrant
	}
	if !cursorUserAPIKey(c.RefreshToken) {
		if cursorAccessUsable(c, now) {
			return ErrCursorNotRotatable
		}
		return ErrInvalidGrant
	}
	_, _, exchangeURL := cursorURLs()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, exchangeURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.RefreshToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: cursor refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	badStatus := resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden
	if badStatus {
		if cursorAccessUsable(c, now) {
			return ErrCursorNotRotatable
		}
		return ErrInvalidGrant
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("oauth: cursor refresh: HTTP %d", resp.StatusCode)
	}

	var payload cursorTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("oauth: cursor refresh: decode %d: %w", resp.StatusCode, err)
	}
	if payload.AccessToken == "" {
		return fmt.Errorf("oauth: cursor refresh: empty accessToken (HTTP %d)", resp.StatusCode)
	}

	c.AccessToken = payload.AccessToken
	if payload.RefreshToken != "" {
		c.RefreshToken = payload.RefreshToken
	}
	if exp := CursorTokenExpiry(c.AccessToken); exp > 0 {
		c.ExpiresAt = exp
	} else {
		c.ExpiresAt = 0
	}
	return nil
}

// CursorTokenExpiry reads the JWT exp claim without verifying the signature.
// It is used only to schedule refresh. Non-JWT / malformed tokens return 0.
func CursorTokenExpiry(accessToken string) int64 {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return int64(claims.Exp)
}

func decodeJWTPayload(seg string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(seg)
}

// cursorPKCE: 32 random bytes, URL-safe base64 without padding; challenge is
// base64url(SHA-256(verifier)) without padding.
func cursorPKCE() (verifier, challenge string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// newCursorMachineID returns a stable UUID identity for x-cursor-checksum.
// Generated once per browser connect and never rotated on refresh.
func newCursorMachineID() (string, error) {
	return newUUIDString()
}
