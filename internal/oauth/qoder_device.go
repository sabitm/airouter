package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/qoder"
)

// Overridable for tests.
var (
	qoderLoginURL       = qoder.LoginURL
	qoderDeviceTokenURL = qoder.DeviceTokenURL
	qoderUserInfoURL    = qoder.UserInfoURL
)

const (
	qoderDevicePollInterval = 2 * time.Second
	qoderDeviceExpiresIn    = 5 * time.Minute
	qoderDefaultTokenTTL    = 30 * 24 * time.Hour
)

// QoderDeviceConnect drives Qoder's custom device flow (PKCE + nonce + machineId).
type QoderDeviceConnect struct {
	state     string
	verifier  string
	nonce     string
	machineID string
	authURL   string

	mu      sync.Mutex
	done    chan struct{}
	result  exchangeResult
	started bool
	cancel  context.CancelFunc
}

// NewQoderDeviceConnect prepares a device-flow attempt.
func NewQoderDeviceConnect() (*QoderDeviceConnect, error) {
	state, err := newState()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := qoderPKCE()
	if err != nil {
		return nil, err
	}
	nonce, err := newUUIDString()
	if err != nil {
		return nil, err
	}
	machineID, err := newUUIDString()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("machine_id", machineID)
	q.Set("nonce", nonce)
	return &QoderDeviceConnect{
		state:     state,
		verifier:  verifier,
		nonce:     nonce,
		machineID: machineID,
		authURL:   qoderLoginURL + "?" + q.Encode(),
		done:      make(chan struct{}),
	}, nil
}

func (d *QoderDeviceConnect) State() string { return d.state }

func (d *QoderDeviceConnect) VerificationURI() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.authURL
}

// Start begins background polling. The user opens VerificationURI in a browser.
func (d *QoderDeviceConnect) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return errors.New("oauth: qoder device connect already started")
	}
	d.started = true
	d.mu.Unlock()

	pollCtx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()

	go d.pollLoop(pollCtx)
	return nil
}

func (d *QoderDeviceConnect) Result() (*domain.OAuthCreds, error, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return d.result.creds, d.result.err, true
	default:
		return nil, nil, false
	}
}

func (d *QoderDeviceConnect) Close() error {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (d *QoderDeviceConnect) pollLoop(ctx context.Context) {
	deadline := time.Now().Add(qoderDeviceExpiresIn)
	ticker := time.NewTicker(devicePollMin)
	if qoderDevicePollInterval > devicePollMin {
		ticker.Reset(qoderDevicePollInterval)
	}
	defer ticker.Stop()

	// First poll immediately.
	if d.pollOnce(ctx) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			d.finish(nil, ctx.Err())
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				d.finish(nil, errors.New("oauth: qoder device authorization timed out"))
				return
			}
			if d.pollOnce(ctx) {
				return
			}
		}
	}
}

// pollOnce returns true when the flow completed (success or terminal error).
func (d *QoderDeviceConnect) pollOnce(ctx context.Context) bool {
	u := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		qoderDeviceTokenURL,
		url.QueryEscape(d.nonce),
		url.QueryEscape(d.verifier),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		d.finish(nil, err)
		return true
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			d.finish(nil, ctx.Err())
			return true
		}
		// Transient network error — keep polling.
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNotFound {
		return false
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("qoder device token poll failed: HTTP %d", resp.StatusCode)
		var env struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &env) == nil && env.Message != "" {
			msg = "qoder device token poll failed: " + env.Message
		}
		d.finish(nil, errors.New(msg))
		return true
	}

	var payload struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		ExpiresAt    any    `json:"expires_at"`
		ExpiresIn    any    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		d.finish(nil, fmt.Errorf("qoder device token poll: invalid JSON: %w", err))
		return true
	}
	if payload.Token == "" {
		d.finish(nil, errors.New("qoder device token poll returned 200 but no token"))
		return true
	}

	creds := &domain.OAuthCreds{
		Mode:         domain.OAuthAuto,
		Preset:       "qoder",
		QoderAuth:    true,
		AccessToken:  payload.Token,
		RefreshToken: payload.RefreshToken,
		UserID:       payload.UserID,
		MachineID:    d.machineID,
		ExpiresAt:    parseQoderExpiry(payload.ExpiresAt, payload.ExpiresIn, time.Now()),
	}
	// Best-effort profile.
	if name, email, org := fetchQoderUserInfo(ctx, payload.Token); name != "" || email != "" || org != "" {
		creds.DisplayName = name
		creds.Email = email
		creds.OrganizationID = org
	}
	d.finish(creds, nil)
	return true
}

func (d *QoderDeviceConnect) finish(creds *domain.OAuthCreds, err error) {
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

func fetchQoderUserInfo(ctx context.Context, accessToken string) (name, email, org string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoderUserInfoURL, nil)
	if err != nil {
		return "", "", ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return "", "", ""
	}
	defer resp.Body.Close()
	var body struct {
		Name           string `json:"name"`
		Username       string `json:"username"`
		Email          string `json:"email"`
		OrganizationID string `json:"organization_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return "", "", ""
	}
	name = strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(body.Username)
	}
	return name, strings.TrimSpace(body.Email), strings.TrimSpace(body.OrganizationID)
}

// parseQoderExpiry converts upstream expiry hints to unix seconds.
// Order: numeric ms epoch (number or digit string) before RFC3339; then expires_in seconds.
func parseQoderExpiry(expiresAt, expiresIn any, now time.Time) int64 {
	switch v := expiresAt.(type) {
	case float64:
		if v > 0 {
			// Heuristic: values > 1e12 are ms.
			if v > 1e12 {
				return int64(v / 1000)
			}
			return int64(v)
		}
	case string:
		s := strings.TrimSpace(v)
		if s != "" {
			if onlyDigits(s) {
				n, err := strconv.ParseInt(s, 10, 64)
				if err == nil && n > 0 {
					if n > 1e12 {
						return n / 1000
					}
					// Could be seconds epoch if already small.
					if n > 1e9 {
						return n
					}
				}
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t.Unix()
			}
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t.Unix()
			}
		}
	}
	switch v := expiresIn.(type) {
	case float64:
		if v >= 0 {
			return now.Add(time.Duration(v) * time.Second).Unix()
		}
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && n >= 0 {
			return now.Add(time.Duration(n) * time.Second).Unix()
		}
	}
	return now.Add(qoderDefaultTokenTTL).Unix()
}

func onlyDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// qoderPKCE: 32 random bytes → base64url verifier; S256 challenge (matches qodercli).
func qoderPKCE() (verifier, challenge string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func newUUIDString() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// refreshQoder always fails: device tokens cannot refresh (center returns 403).
func refreshQoder(_ context.Context, _ *domain.OAuthCreds, _ time.Time) error {
	return ErrInvalidGrant
}
