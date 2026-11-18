package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/opencode"
)

const opencodeDeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// opencodeConsoleHTTP is the client for Console device, refresh, account, and
// workspace calls. It refuses redirects so a device code or token is not sent
// to another host. Tests may replace it and must restore it. Forms cannot.
var opencodeConsoleHTTP = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("oauth: redirect refused")
	},
}

// opencodeConsoleNow is the clock for device expiry. Tests may replace it.
var opencodeConsoleNow = time.Now

// SetOpencodeConsoleClientForTest replaces the Console HTTP client. The
// returned function restores it. Dashboard forms cannot set this client.
func SetOpencodeConsoleClientForTest(c *http.Client) func() {
	prev := opencodeConsoleHTTP
	if c == nil {
		c = http.DefaultClient
	}
	opencodeConsoleHTTP = c
	return func() { opencodeConsoleHTTP = prev }
}

// SetOpencodeConsoleWaitForTest replaces the device-poll wait. The returned
// function restores it. Production waits the server interval.
func SetOpencodeConsoleWaitForTest(fn func(context.Context, time.Duration) error) func() {
	prev := opencodeConsoleWait
	if fn == nil {
		fn = prev
	}
	opencodeConsoleWait = fn
	return func() { opencodeConsoleWait = prev }
}

// opencodeConsoleWait sleeps between device polls. Tests may replace it so
// interval and slow_down behavior can be observed without waiting.
var opencodeConsoleWait = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var opencodeSafeErrorCode = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// OpencodeWorkspace is one account workspace offered for an explicit choice.
// It does not contain tokens.
type OpencodeWorkspace struct {
	ID   string
	Name string
}

type opencodeDeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type opencodeTokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    int     `json:"expires_in"`
	OrgID        *string `json:"org_id"`
	Error        string  `json:"error"`
}

type opencodeUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type opencodeOrg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OpencodeConsoleConnect drives the OpenCode Console device grant. Production
// endpoints are fixed. The verification URL may be relative to the Console
// origin. Device codes and tokens are not returned in errors or status text.
type OpencodeConsoleConnect struct {
	state string

	mu       sync.Mutex
	done     chan struct{}
	result   exchangeResult
	cancel   context.CancelFunc
	started  bool
	phase    string
	userCode string
	verify   string
	interval time.Duration
	expires  time.Time

	access    string
	refresh   string
	expiresAt int64
	email     string
	accountID string
	orgs      []OpencodeWorkspace
}

// NewOpencodeConsoleConnect starts a bound device attempt. Start must be called
// before the verification URL is shown.
func NewOpencodeConsoleConnect() (*OpencodeConsoleConnect, error) {
	state, err := newState()
	if err != nil {
		return nil, err
	}
	return &OpencodeConsoleConnect{
		state: state,
		done:  make(chan struct{}),
		phase: "pending",
	}, nil
}

func (d *OpencodeConsoleConnect) State() string { return d.state }

func (d *OpencodeConsoleConnect) UserCode() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.userCode
}

func (d *OpencodeConsoleConnect) VerificationURI() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.verify
}

// Status reports the attempt without tokens. phase is pending, choice,
// connected, or error. orgs is set only while a workspace choice is required.
func (d *OpencodeConsoleConnect) Status() (phase, workspace, errMsg string, orgs []OpencodeWorkspace) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		if d.result.err != nil {
			return "error", "", d.result.err.Error(), nil
		}
		name := ""
		if d.result.creds != nil {
			name = d.result.creds.OpencodeOrgName
		}
		return "connected", name, "", nil
	default:
	}
	if d.phase == "choice" {
		out := append([]OpencodeWorkspace(nil), d.orgs...)
		return "choice", "", "", out
	}
	return "pending", "", "", nil
}

func (d *OpencodeConsoleConnect) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return errors.New("oauth: device connect already started")
	}
	d.started = true
	d.mu.Unlock()

	da, err := d.requestDeviceCode(ctx)
	if err != nil {
		return err
	}
	verify, err := resolveOpencodeVerificationURL(opencode.ConsoleOrigin, da.VerificationURIComplete, da.DeviceCode)
	if err != nil {
		return err
	}
	if da.ExpiresIn <= 0 {
		return errors.New("oauth: device code expired")
	}
	interval := time.Duration(da.Interval) * time.Second
	if interval < 0 {
		interval = 0
	}

	d.mu.Lock()
	d.userCode = da.UserCode
	d.verify = verify
	d.interval = interval
	d.expires = opencodeConsoleNow().Add(time.Duration(da.ExpiresIn) * time.Second)
	d.mu.Unlock()

	pollCtx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
	go d.pollLoop(pollCtx, da.DeviceCode)
	return nil
}

func (d *OpencodeConsoleConnect) requestDeviceCode(ctx context.Context) (*opencodeDeviceCodeResponse, error) {
	body, _ := json.Marshal(map[string]any{
		"client_id":          opencode.ConsoleClientID,
		"supports_org_scope": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opencode.ConsoleDeviceURL(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("oauth: device authorization failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := opencodeConsoleHTTP.Do(req)
	if err != nil {
		return nil, errors.New("oauth: device authorization failed")
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("oauth: device authorization failed")
	}
	var da opencodeDeviceCodeResponse
	if err := json.Unmarshal(raw, &da); err != nil || da.DeviceCode == "" || da.VerificationURIComplete == "" {
		return nil, errors.New("oauth: device authorization failed")
	}
	return &da, nil
}

func (d *OpencodeConsoleConnect) pollLoop(ctx context.Context, deviceCode string) {
	d.mu.Lock()
	interval := d.interval
	expires := d.expires
	d.mu.Unlock()

	for {
		wait := interval
		if wait < devicePollMin {
			wait = devicePollMin
		}
		if err := opencodeConsoleWait(ctx, wait); err != nil {
			d.finishWithErr(errors.New("oauth: device authorization canceled"))
			return
		}
		if opencodeConsoleNow().After(expires) {
			d.finishWithErr(errors.New("oauth: device code expired"))
			return
		}
		tr, kind, err := d.pollOnce(ctx, deviceCode)
		if err != nil {
			d.finishWithErr(err)
			return
		}
		switch kind {
		case "pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "token":
			if err := d.completeToken(ctx, tr); err != nil {
				d.finishWithErr(err)
				return
			}
			return
		default:
			d.finishWithErr(opencodeTerminalError(tr.Error, deviceCode))
			return
		}
	}
}

func (d *OpencodeConsoleConnect) pollOnce(ctx context.Context, deviceCode string) (opencodeTokenResponse, string, error) {
	var tr opencodeTokenResponse
	body, _ := json.Marshal(map[string]string{
		"grant_type":  opencodeDeviceGrant,
		"device_code": deviceCode,
		"client_id":   opencode.ConsoleClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opencode.ConsoleTokenURL(), bytes.NewReader(body))
	if err != nil {
		return tr, "", errors.New("oauth: device authorization failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := opencodeConsoleHTTP.Do(req)
	if err != nil {
		return tr, "", errors.New("oauth: device authorization failed")
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if err := json.Unmarshal(raw, &tr); err != nil {
		return tr, "", errors.New("oauth: device authorization failed")
	}
	if tr.AccessToken != "" {
		return tr, "token", nil
	}
	switch tr.Error {
	case "authorization_pending":
		return tr, "pending", nil
	case "slow_down":
		return tr, "slow_down", nil
	case "":
		return tr, "", errors.New("oauth: device authorization failed")
	default:
		return tr, "terminal", nil
	}
}

func (d *OpencodeConsoleConnect) completeToken(ctx context.Context, tr opencodeTokenResponse) error {
	if tr.RefreshToken == "" || tr.ExpiresIn <= 0 {
		return errors.New("oauth: device authorization failed")
	}
	user, orgs, err := opencodeAccount(ctx, tr.AccessToken)
	if err != nil {
		return err
	}
	expiresAt := opencodeConsoleNow().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	orgID := ""
	if tr.OrgID != nil {
		orgID = strings.TrimSpace(*tr.OrgID)
	}
	if orgID != "" {
		org, ok := findOpencodeOrg(orgs, orgID)
		if !ok {
			return errors.New("oauth: OpenCode organization was not found")
		}
		d.finishWithCreds(opencodeConsoleCreds(tr.AccessToken, tr.RefreshToken, expiresAt, user, org))
		return nil
	}
	switch len(orgs) {
	case 0:
		return errors.New("oauth: OpenCode Console returned no workspace")
	case 1:
		d.finishWithCreds(opencodeConsoleCreds(tr.AccessToken, tr.RefreshToken, expiresAt, user, orgs[0]))
		return nil
	default:
		d.mu.Lock()
		defer d.mu.Unlock()
		select {
		case <-d.done:
			return nil
		default:
		}
		d.access = tr.AccessToken
		d.refresh = tr.RefreshToken
		d.expiresAt = expiresAt
		d.email = user.Email
		d.accountID = user.ID
		d.orgs = orgs
		d.phase = "choice"
		return nil
	}
}

// ChooseWorkspace binds an explicit workspace for a grant that omitted org_id.
// The id must be one of the workspaces returned for this attempt. A mismatch
// does not substitute another workspace and leaves the choice open.
func (d *OpencodeConsoleConnect) ChooseWorkspace(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.phase != "choice" {
		return errors.New("oauth: workspace choice is not available")
	}
	org, ok := findOpencodeOrg(d.orgs, strings.TrimSpace(id))
	if !ok {
		return errors.New("oauth: OpenCode workspace is not available for this account")
	}
	d.phase = "connected"
	d.result = exchangeResult{creds: opencodeConsoleCreds(d.access, d.refresh, d.expiresAt, opencodeUser{ID: d.accountID, Email: d.email}, org)}
	d.access = ""
	d.refresh = ""
	close(d.done)
	return nil
}

func (d *OpencodeConsoleConnect) finishWithCreds(creds *domain.OAuthCreds) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return
	default:
		d.phase = "connected"
		d.access = ""
		d.refresh = ""
		d.result = exchangeResult{creds: creds}
		close(d.done)
	}
}

func (d *OpencodeConsoleConnect) finishWithErr(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return
	default:
		d.phase = "error"
		d.access = ""
		d.refresh = ""
		d.result = exchangeResult{err: err}
		close(d.done)
	}
}

func (d *OpencodeConsoleConnect) Result() (creds *domain.OAuthCreds, err error, done bool) {
	select {
	case <-d.done:
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.result.creds, d.result.err, true
	default:
		return nil, nil, false
	}
}

func (d *OpencodeConsoleConnect) Close() error {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func opencodeConsoleCreds(access, refresh string, expiresAt int64, user opencodeUser, org OpencodeWorkspace) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode:                domain.OAuthAuto,
		Preset:              "opencode-console",
		AccessToken:         access,
		RefreshToken:        refresh,
		ExpiresAt:           expiresAt,
		Email:               user.Email,
		AccountID:           user.ID,
		ClientID:            opencode.ConsoleClientID,
		TokenURL:            "https://opencode.ai/console/auth/device/token",
		RefreshJSON:         true,
		OpencodeConsoleAuth: true,
		OpencodeOrgID:       org.ID,
		OpencodeOrgName:     org.Name,
	}
}

func opencodeAccount(ctx context.Context, access string) (opencodeUser, []OpencodeWorkspace, error) {
	var user opencodeUser
	var orgs []opencodeOrg
	if err := opencodeGetJSON(ctx, opencode.ConsoleUserURL(), access, &user); err != nil {
		return user, nil, errors.New("oauth: OpenCode Console account lookup failed")
	}
	if err := opencodeGetJSON(ctx, opencode.ConsoleOrgsURL(), access, &orgs); err != nil {
		return user, nil, errors.New("oauth: OpenCode Console workspace lookup failed")
	}
	out := make([]OpencodeWorkspace, 0, len(orgs))
	seen := map[string]bool{}
	for _, org := range orgs {
		id := strings.TrimSpace(org.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, OpencodeWorkspace{ID: id, Name: strings.TrimSpace(org.Name)})
	}
	return user, out, nil
}

func opencodeGetJSON(ctx context.Context, rawURL, access string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := opencodeConsoleHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("oauth: console lookup HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return err
	}
	return nil
}

func findOpencodeOrg(orgs []OpencodeWorkspace, id string) (OpencodeWorkspace, bool) {
	for _, org := range orgs {
		if org.ID == id {
			return org, true
		}
	}
	return OpencodeWorkspace{}, false
}

func resolveOpencodeVerificationURL(server, raw, deviceCode string) (string, error) {
	base, err := url.Parse(strings.TrimRight(server, "/") + "/")
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return "", errors.New("oauth: invalid device verification URL")
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(raw) == "" {
		return "", errors.New("oauth: invalid device verification URL")
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "https" && resolved.Scheme != "http" {
		return "", errors.New("oauth: invalid device verification URL")
	}
	if verificationContainsDeviceCode(resolved, deviceCode) {
		return "", errors.New("oauth: invalid device verification URL")
	}
	return resolved.String(), nil
}

func verificationContainsDeviceCode(u *url.URL, deviceCode string) bool {
	if u == nil || deviceCode == "" {
		return false
	}
	for key, vals := range u.Query() {
		if key == "device_code" {
			return true
		}
		for _, val := range vals {
			if val == deviceCode {
				return true
			}
		}
	}
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if seg == deviceCode {
			return true
		}
	}
	return false
}

func opencodeTerminalError(code, deviceCode string) error {
	if code == "" || code == deviceCode || !opencodeSafeErrorCode.MatchString(code) {
		return errors.New("oauth: device authorization failed")
	}
	return fmt.Errorf("oauth: device authorization failed: %s", code)
}

// refreshOpencodeConsole exchanges a Console refresh token at the fixed device
// token URL. A stored TokenURL is ignored. A returned org_id that differs from
// the bound workspace is a reconnect, not a silent billing switch.
func refreshOpencodeConsole(ctx context.Context, c *domain.OAuthCreds, now time.Time) error {
	if c == nil || !c.OpencodeConsoleAuth {
		return errors.New("oauth: OpenCode Console credentials are incomplete")
	}
	if strings.TrimSpace(c.RefreshToken) == "" || strings.TrimSpace(c.OpencodeOrgID) == "" {
		return errors.New("oauth: OpenCode Console credentials are incomplete")
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"client_id":     opencode.ConsoleClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opencode.ConsoleTokenURL(), bytes.NewReader(body))
	if err != nil {
		return errors.New("oauth: refresh request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := opencodeConsoleHTTP.Do(req)
	if err != nil {
		return errors.New("oauth: refresh request failed")
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	var tr opencodeTokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return ErrInvalidGrant
		}
		return errors.New("oauth: refresh response was not valid")
	}
	if tr.Error == "invalid_grant" || resp.StatusCode == http.StatusUnauthorized {
		return ErrInvalidGrant
	}
	if tr.Error != "" || resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.AccessToken == "" || tr.RefreshToken == "" || tr.ExpiresIn <= 0 {
		return errors.New("oauth: refresh response was not valid")
	}
	if tr.OrgID != nil {
		got := strings.TrimSpace(*tr.OrgID)
		if got != "" && got != c.OpencodeOrgID {
			return fmt.Errorf("%w: OpenCode Console workspace changed", ErrInvalidGrant)
		}
	}
	c.AccessToken = tr.AccessToken
	c.RefreshToken = tr.RefreshToken
	c.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	c.ClientID = opencode.ConsoleClientID
	c.TokenURL = opencode.ConsoleTokenURL()
	c.RefreshJSON = true
	return nil
}
