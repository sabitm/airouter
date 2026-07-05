package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
)

const (
	kiroDeviceClientName   = "kiro-oauth-client"
	kiroDeviceClientType   = "public"
	kiroDeviceStartURL     = "https://view.awsapps.com/start"
	kiroDeviceIssuerURL    = "https://identitycenter.amazonaws.com/ssoins-722374e8c3c8e6c6"
	kiroDeviceGrantType    = "urn:ietf:params:oauth:grant-type:device_code"
	kiroBuilderIDPreset    = "kiro"
	kiroBuilderIDAuth      = "builder-id"
)

var kiroDeviceScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
}

var kiroDeviceGrantTypes = []string{
	kiroDeviceGrantType,
	"refresh_token",
}

// kiroAWSRegionRe guards region interpolation into AWS hostnames (SSRF).
var kiroAWSRegionRe = regexp.MustCompile(`^[a-z]{2}-[a-z]+-\d{1,2}$`)

var (
	kiroRegisterURL = func(region string) string {
		if region == "" {
			region = "us-east-1"
		}
		return fmt.Sprintf("https://oidc.%s.amazonaws.com/client/register", region)
	}
	kiroDeviceAuthURL = func(region string) string {
		if region == "" {
			region = "us-east-1"
		}
		return fmt.Sprintf("https://oidc.%s.amazonaws.com/device_authorization", region)
	}
	kiroListProfilesURL = func(region string) string {
		if region == "" {
			region = "us-east-1"
		}
		return fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/", region)
	}
)

// devicePollMin is the minimum delay between token polls; tests may lower it.
var devicePollMin = time.Second

func validateKiroRegion(region string) (string, error) {
	if region == "" {
		return "us-east-1", nil
	}
	if !kiroAWSRegionRe.MatchString(region) {
		return "", fmt.Errorf("oauth: invalid AWS region %q", region)
	}
	return region, nil
}

type deviceAuthResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

type kiroRegisterResponse struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// DeviceConnect drives the AWS SSO OIDC device authorization grant for Kiro Builder ID.
type DeviceConnect struct {
	region string
	state  string

	mu     sync.Mutex
	done   chan struct{}
	result exchangeResult
	started bool

	cancel context.CancelFunc

	deviceCode   string
	clientID     string
	clientSecret string
	interval     time.Duration
	expiresAt    time.Time

	userCode                string
	verificationURIComplete string
}

func NewDeviceConnect(region string) (*DeviceConnect, error) {
	resolved, err := validateKiroRegion(strings.TrimSpace(region))
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, err
	}
	return &DeviceConnect{
		region: resolved,
		state:  state,
		done:   make(chan struct{}),
	}, nil
}

func (d *DeviceConnect) State() string { return d.state }

func (d *DeviceConnect) UserCode() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.userCode
}

func (d *DeviceConnect) VerificationURI() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.verificationURIComplete
}

func (d *DeviceConnect) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return errors.New("oauth: device connect already started")
	}
	d.mu.Unlock()

	clientID, clientSecret, err := d.registerClient(ctx)
	if err != nil {
		return err
	}
	da, err := d.requestDeviceAuth(ctx, clientID, clientSecret)
	if err != nil {
		return err
	}

	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresIn := da.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}

	d.mu.Lock()
	d.clientID = clientID
	d.clientSecret = clientSecret
	d.deviceCode = da.DeviceCode
	d.interval = interval
	d.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	d.userCode = da.UserCode
	d.verificationURIComplete = da.VerificationURIComplete
	if d.verificationURIComplete == "" {
		d.verificationURIComplete = da.VerificationURI
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

func (d *DeviceConnect) registerClient(ctx context.Context) (clientID, clientSecret string, err error) {
	body, _ := json.Marshal(map[string]any{
		"clientName": kiroDeviceClientName,
		"clientType": kiroDeviceClientType,
		"scopes":     kiroDeviceScopes,
		"grantTypes": kiroDeviceGrantTypes,
		"issuerUrl":  kiroDeviceIssuerURL,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroRegisterURL(d.region), bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("oauth: kiro register: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	var rr kiroRegisterResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return "", "", fmt.Errorf("oauth: kiro register: decode %d: %w", resp.StatusCode, err)
	}
	if rr.ClientID == "" || rr.ClientSecret == "" {
		return "", "", fmt.Errorf("oauth: kiro register: missing client credentials (HTTP %d)", resp.StatusCode)
	}
	return rr.ClientID, rr.ClientSecret, nil
}

func (d *DeviceConnect) requestDeviceAuth(ctx context.Context, clientID, clientSecret string) (*deviceAuthResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"startUrl":     kiroDeviceStartURL,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroDeviceAuthURL(d.region), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: kiro device_authorization: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	var da deviceAuthResponse
	if err := json.Unmarshal(raw, &da); err != nil {
		return nil, fmt.Errorf("oauth: kiro device_authorization: decode %d: %w", resp.StatusCode, err)
	}
	if da.DeviceCode == "" {
		return nil, fmt.Errorf("oauth: kiro device_authorization: empty deviceCode (HTTP %d)", resp.StatusCode)
	}
	return &da, nil
}

func (d *DeviceConnect) pollLoop(ctx context.Context) {
	d.mu.Lock()
	interval := d.interval
	expiresAt := d.expiresAt
	d.mu.Unlock()

	for {
		wait := interval
		if wait < devicePollMin {
			wait = devicePollMin
		}
		select {
		case <-ctx.Done():
			d.finishWithErr(ctx.Err())
			return
		case <-time.After(wait):
		}

		if time.Now().After(expiresAt) {
			d.finishWithErr(errors.New("oauth: device code expired"))
			return
		}

		tr, pending, slowDown, terminal, err := d.pollOnce(ctx)
		if err != nil {
			d.finishWithErr(err)
			return
		}
		if pending {
			continue
		}
		if slowDown {
			interval += 5 * time.Second
			if interval > 30*time.Second {
				interval = 30 * time.Second
			}
			continue
		}
		if terminal {
			d.finishWithErr(fmt.Errorf("oauth: device grant: %s", tr.Error))
			return
		}
		if tr.AccessToken != "" {
			creds, err := d.buildCreds(ctx, tr)
			if err != nil {
				d.finishWithErr(err)
				return
			}
			d.finishWithCreds(creds)
			return
		}
	}
}

func (d *DeviceConnect) pollOnce(ctx context.Context) (tr kiroTokenResponse, pending, slowDown, terminal bool, err error) {
	d.mu.Lock()
	clientID, clientSecret, deviceCode, region := d.clientID, d.clientSecret, d.deviceCode, d.region
	d.mu.Unlock()

	body, _ := json.Marshal(map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"deviceCode":   deviceCode,
		"grantType":    kiroDeviceGrantType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroOIDCTokenURL(region), bytes.NewReader(body))
	if err != nil {
		return tr, false, false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return tr, false, false, false, fmt.Errorf("oauth: kiro device token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	if err := json.Unmarshal(raw, &tr); err != nil {
		return tr, false, false, false, fmt.Errorf("oauth: kiro device token: decode %d: %w", resp.StatusCode, err)
	}

	if tr.AccessToken != "" {
		return tr, false, false, false, nil
	}
	switch strings.ToLower(tr.Error) {
	case "authorization_pending":
		return tr, true, false, false, nil
	case "slow_down":
		return tr, false, true, false, nil
	case "expired_token", "access_denied", "invalid_grant":
		return tr, false, false, true, nil
	default:
		if tr.Error != "" {
			return tr, false, false, true, nil
		}
		return tr, true, false, false, nil
	}
}

func (d *DeviceConnect) buildCreds(ctx context.Context, tr kiroTokenResponse) (*domain.OAuthCreds, error) {
	d.mu.Lock()
	clientID, clientSecret, region := d.clientID, d.clientSecret, d.region
	d.mu.Unlock()

	now := time.Now()
	c := &domain.OAuthCreds{
		Mode:         domain.OAuthAuto,
		Preset:       kiroBuilderIDPreset,
		KiroAuth:     kiroBuilderIDAuth,
		Region:       region,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ProfileArn:   tr.ProfileArn,
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	if email, _, ok := ClaimsFromToken(tr.AccessToken); ok && email != "" {
		c.Email = email
	}
	if c.ProfileArn == "" {
		if arn, err := resolveKiroProfileArn(ctx, region, tr.AccessToken); err == nil && arn != "" {
			c.ProfileArn = arn
		}
	}
	return c, nil
}

func resolveKiroProfileArn(ctx context.Context, region, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroListProfilesURL(region), bytes.NewReader([]byte(`{"maxResults":10}`)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("x-amz-target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("ListAvailableProfiles HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Profiles []struct {
			Arn        string `json:"arn"`
			ProfileArn string `json:"profileArn"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	for _, p := range parsed.Profiles {
		if p.ProfileArn != "" {
			return p.ProfileArn, nil
		}
		if p.Arn != "" {
			return p.Arn, nil
		}
	}
	return "", nil
}

func (d *DeviceConnect) finishWithCreds(creds *domain.OAuthCreds) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return
	default:
		d.result = exchangeResult{creds: creds}
		close(d.done)
	}
}

func (d *DeviceConnect) finishWithErr(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return
	default:
		d.result = exchangeResult{err: err}
		close(d.done)
	}
}

func (d *DeviceConnect) Result() (creds *domain.OAuthCreds, err error, done bool) {
	select {
	case <-d.done:
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.result.creds, d.result.err, true
	default:
		return nil, nil, false
	}
}

func (d *DeviceConnect) Wait(ctx context.Context) (*domain.OAuthCreds, error) {
	select {
	case <-d.done:
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.result.creds, d.result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *DeviceConnect) Close() error {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}