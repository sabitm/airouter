// Package grokbuild is the Grok Build CLI subscription backend.
// It is not the public xAI API. Tokens from this flow are sent only to the
// fixed cli-chat-proxy host, and refresh material is sent only to auth.x.ai.
//
// Wire facts are taken from the public grok-build tree at commit
// 2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8 (main, no GitHub releases, version
// crate 1.0.45). That tree can be older than a locally installed CLI. A 426
// from the proxy is reported as a version mismatch against this pin. It is not
// a live version probe and not a claim of CLI parity.
package grokbuild

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
)

const (
	// Version is xai-grok-version's Cargo.toml version at SourceCommit.
	Version = "1.0.45"
	// SourceCommit is the grok-build commit this pin was read from.
	SourceCommit = "2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8"

	BaseURL       = "https://cli-chat-proxy.grok.com/v1"
	ModelsURL     = BaseURL + "/models"
	ResponsesPath = "/responses"
	// BillingURL is the read-only credits endpoint from the official CLI.
	// It is not an inference URL. DoFixed and RejectUnsafeSend do not accept it.
	BillingURL    = BaseURL + "/billing?format=credits"
	Issuer        = "https://auth.x.ai"
	DeviceCodeURL = Issuer + "/oauth2/device/code"
	TokenURL      = Issuer + "/oauth2/token"

	// ClientID is the public xAI OAuth client embedded in grok-build. It is the
	// same id the public xAI preset uses. A shared id must not share tokens.
	ClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// Scopes is the frozen personal OAuth2 scope list from xai-grok-login config.rs.
	Scopes   = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"
	Referrer = "grok-build"
	Preset   = "grok-build"

	TokenAuthHeader = "X-XAI-Token-Auth"
	TokenAuthValue  = "xai-grok-cli"
	ClientProduct   = "grok-shell"
	// ClientMode is the official default. The CLI sets headless only after an
	// explicit process call. This proxy does not make that call.
	ClientMode = "interactive"
	// ClientSurface is the device-flow metric for a human-rendered login. The
	// dashboard shows the URL and code, so this is ui rather than cli or headless.
	ClientSurface = "ui"

	inferenceHost = "cli-chat-proxy.grok.com"
	authHost      = "auth.x.ai"
	accountsHost  = "accounts.x.ai"
)

const billingBodyLimit = 1 << 20

var (
	errUnsafeHost       = errors.New("grok-build: credentials are only sent to the fixed subscription host")
	errBadCreds         = errors.New("grok-build: credentials are not valid for this provider")
	errRedirectRefused  = errors.New("grok-build: redirect refused")
	errBillingTransport = errors.New("grok-build: billing request failed")
	errBillingRedirect  = errors.New("grok-build: redirect refused")
	errBillingOversize  = errors.New("grok-build: billing response was too large")
	errBillingRead      = errors.New("grok-build: billing response read failed")
)

// Marked reports a subscription credential. The bool and the preset are each
// sufficient. A preset-only row is not a generic provider.
func Marked(c *domain.OAuthCreds) bool {
	return c != nil && (c.GrokBuildAuth || c.Preset == Preset)
}

// Involved reports a row that must use the subscription backstops. Protocol,
// marker, and preset are each sufficient.
func Involved(p *domain.Provider) bool {
	return p != nil && (p.Protocol == domain.ProtocolGrokBuild || Marked(p.OAuthCreds))
}

// VersionMismatchError is the sanitized 426 result. It does not include the
// upstream body.
func VersionMismatchError() error {
	return fmt.Errorf("grok-build: upstream rejected client version %s (HTTP 426); pinned from grok-build commit %s", Version, SourceCommit[:12])
}

// UserAgent matches the official grok-shell renderer. Go arch and OS names are
// mapped onto the names that renderer emits.
func UserAgent() string {
	return fmt.Sprintf("%s/%s (%s; %s)", ClientProduct, Version, userAgentOS(), userAgentArch())
}

func userAgentOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func userAgentArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// IsFixedBase reports whether raw is the subscription API base and nothing else.
func IsFixedBase(raw string) bool {
	return sameFixedURL(raw, BaseURL)
}

// IsFixedInferenceURL reports whether raw is /v1/responses or /v1/models on the
// subscription host, with no query, fragment, or userinfo.
func IsFixedInferenceURL(raw string) bool {
	return sameFixedURL(raw, ModelsURL) || sameFixedURL(raw, BaseURL+ResponsesPath)
}

// IsFixedDeviceURL reports whether raw is the device-code endpoint.
func IsFixedDeviceURL(raw string) bool {
	return sameAuthURL(raw, DeviceCodeURL)
}

// IsFixedTokenURL reports whether raw is the token endpoint.
func IsFixedTokenURL(raw string) bool {
	return sameAuthURL(raw, TokenURL)
}

func sameFixedURL(raw, want string) bool {
	return sameHostURL(raw, want, inferenceHost)
}

func sameAuthURL(raw, want string) bool {
	return sameHostURL(raw, want, authHost)
}

func sameHostURL(raw, want, host string) bool {
	left, err1 := url.Parse(strings.TrimSpace(raw))
	right, err2 := url.Parse(want)
	if err1 != nil || err2 != nil || left.User != nil || left.RawQuery != "" || left.Fragment != "" {
		return false
	}
	if !strings.EqualFold(left.Scheme, "https") || !strings.EqualFold(left.Hostname(), host) {
		return false
	}
	if port := left.Port(); port != "" && port != "443" {
		return false
	}
	return strings.TrimRight(left.EscapedPath(), "/") == strings.TrimRight(right.EscapedPath(), "/")
}

// ValidateVerificationURI accepts only https URLs on the first-party hosts named
// by the public source (auth.x.ai and accounts.x.ai). The official CLI accepts
// any https URL. This dashboard does not. Localhost is rejected because this
// flow never uses the local issuer.
func ValidateVerificationURI(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\\\r\n\t ") {
		return errors.New("grok-build: verification URL is not an expected https link")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return errors.New("grok-build: verification URL is not an expected https link")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Hostname() == "" {
		return errors.New("grok-build: verification URL is not an expected https link")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errors.New("grok-build: verification URL is not an expected https link")
	}
	if port := u.Port(); port != "" && port != "443" {
		return errors.New("grok-build: verification URL is not an expected https link")
	}
	switch strings.ToLower(u.Hostname()) {
	case authHost, accountsHost:
		return nil
	default:
		return errors.New("grok-build: verification URL is not an expected https link")
	}
}

// RejectMixedGrant fails closed when one credential carries both a Grok Build
// marker and the OpenCode Console marker. Refresh tests the Console marker
// first, so a mixed grant must not reach either token endpoint. The error
// does not include a secret or an upstream body.
func RejectMixedGrant(c *domain.OAuthCreds) error {
	if c == nil || !c.OpencodeConsoleAuth || !Marked(c) {
		return nil
	}
	return errBadCreds
}

// ValidateProvider rejects an imported or saved row that would attach these
// credentials to another host, protocol, or auth method. A preset-only row is
// not treated as generic. A mixed OpenCode Console marker fails closed.
func ValidateProvider(p *domain.Provider) error {
	if !Involved(p) {
		return nil
	}
	if err := RejectMixedGrant(p.OAuthCreds); err != nil {
		return err
	}
	if !Marked(p.OAuthCreds) || p.Protocol != domain.ProtocolGrokBuild || p.Method() != domain.AuthOAuth || p.Auth() != domain.AuthBearer {
		return errBadCreds
	}
	if strings.TrimSpace(p.APIKey) != "" {
		return errors.New("grok-build: credentials cannot be combined with an API key")
	}
	if !IsFixedBase(p.BaseURL) {
		return errUnsafeHost
	}
	c := p.OAuthCreds
	if c.Preset != "" && c.Preset != Preset {
		return errBadCreds
	}
	if c.ClientID != "" && c.ClientID != ClientID {
		return errors.New("grok-build: client id is not the subscription client")
	}
	if c.TokenURL != "" && !IsFixedTokenURL(c.TokenURL) {
		return errors.New("grok-build: token URL is not the official endpoint")
	}
	if c.AuthURL != "" && !sameAuthURL(c.AuthURL, Issuer) {
		return errors.New("grok-build: authorization URL is not the official issuer")
	}
	if c.RefreshURL != "" || c.ClientSecret != "" {
		return errors.New("grok-build: client secret and refresh URL are not configurable")
	}
	if strings.TrimSpace(c.AccessToken) == "" || strings.TrimSpace(c.RefreshToken) == "" {
		return errors.New("grok-build: credentials are incomplete")
	}
	if c.Scopes != "" && c.Scopes != Scopes {
		return errors.New("grok-build: scopes are not the subscription set")
	}
	return nil
}

// ValidateCredentialUse is the request-time backstop. A wrong host, a missing
// marker, or a mixed OpenCode Console marker fails before a token or refresh
// request is built.
func ValidateCredentialUse(p *domain.Provider) error {
	if p == nil {
		return errBadCreds
	}
	if !Involved(p) {
		return nil
	}
	if err := RejectMixedGrant(p.OAuthCreds); err != nil {
		return err
	}
	if p.Protocol != domain.ProtocolGrokBuild || !Marked(p.OAuthCreds) || p.Method() != domain.AuthOAuth || p.Auth() != domain.AuthBearer {
		return errBadCreds
	}
	if !IsFixedBase(p.BaseURL) {
		return errUnsafeHost
	}
	c := p.OAuthCreds
	if c.ClientID != "" && c.ClientID != ClientID {
		return errors.New("grok-build: client id is not the subscription client")
	}
	if c.TokenURL != "" && !IsFixedTokenURL(c.TokenURL) {
		return errors.New("grok-build: token URL is not the official endpoint")
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return errors.New("grok-build: access token is missing")
	}
	return nil
}

// RejectUnsafeSend fails before a subscription bearer is attached to a request
// whose URL is not the fixed inference host.
func RejectUnsafeSend(p *domain.Provider, rawURL string) error {
	if p == nil || !Involved(p) {
		return nil
	}
	if err := ValidateCredentialUse(p); err != nil {
		return err
	}
	token := strings.TrimSpace(p.APIKey)
	if token == "" && p.OAuthCreds != nil {
		token = strings.TrimSpace(p.OAuthCreds.AccessToken)
	}
	if token == "" {
		return errors.New("grok-build: access token is missing")
	}
	if !IsFixedInferenceURL(rawURL) {
		return errUnsafeHost
	}
	return nil
}

// HeaderIDs are request-local upstream identifiers. They are hashes of the
// tenant, provider, and client affinity. They are not a process-wide current
// request and they are not account emails.
type HeaderIDs struct {
	SessionID string
	ConvID    string
	ReqID     string
	AgentID   string
}

// NewReqID returns a request-local id. Callers store it on the request context
// so a retry of that request reuses it.
func NewReqID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ScopedIDs derives upstream ids. clientIdentity empty means this request has
// no durable affinity; the reqID then keeps the scope local to the request.
// Session and conversation share one hash. The official CLI can set a separate
// conversation group. This proxy does not invent that optional root.
func ScopedIDs(tenant string, providerID int64, clientIdentity, reqID string) HeaderIDs {
	if tenant == "" {
		tenant = "open"
	}
	if reqID == "" {
		reqID = NewReqID()
	}
	affinity := clientIdentity
	if affinity == "" {
		affinity = "req:" + reqID
	}
	providerScope := "id:" + fmt.Sprintf("%d", providerID)
	session := hashScope("grok-build-session-v1", tenant, providerScope, affinity)
	return HeaderIDs{
		SessionID: session,
		ConvID:    session,
		ReqID:     reqID,
		AgentID:   hashScope("grok-build-agent-v1", providerScope),
	}
}

func hashScope(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// NormalizeIdentity keeps a single header or body affinity token. Control
// characters and overlong values are dropped rather than forwarded.
func NormalizeIdentity(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 256 {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return ""
		}
	}
	return v
}

// ApplyHeaders sets the subscription inference headers and removes caller
// identity and auth headers. model must be the combo-rewritten model already
// written into body. accountUserID is the token principal the official catalog
// sends as x-userid. Email is never forwarded.
func ApplyHeaders(h http.Header, body []byte, model, accountUserID string, ids HeaderIDs) {
	if h == nil {
		return
	}
	for _, name := range []string{
		"X-Api-Key", "X-Email", "X-Userid", "X-User-Id",
		TokenAuthHeader, "X-Authenticateresponse",
		"X-Grok-Client-Version", "X-Grok-Client-Identifier", "X-Grok-Client-Mode",
		"X-Grok-Model-Override", "X-Grok-Conv-Id", "X-Grok-Req-Id",
		"X-Grok-Session-Id", "X-Grok-Agent-Id", "X-Grok-Turn-Idx",
		"X-Grok-Conv-Group-Id", "X-Grok-User-Id", "X-Grok-Deployment-Id",
		"X-Grok-Transient-Retry",
	} {
		h.Del(name)
	}
	h.Set("User-Agent", UserAgent())
	h.Set(TokenAuthHeader, TokenAuthValue)
	h.Set("X-Authenticateresponse", "authenticate-response")
	h.Set("X-Grok-Client-Version", Version)
	h.Set("X-Grok-Client-Identifier", ClientProduct)
	h.Set("X-Grok-Client-Mode", ClientMode)
	if accountUserID != "" {
		h.Set("X-Userid", accountUserID)
	}
	if model != "" {
		h.Set("X-Grok-Model-Override", model)
	}
	if ids.ConvID != "" {
		h.Set("X-Grok-Conv-Id", ids.ConvID)
	}
	if ids.ReqID != "" {
		h.Set("X-Grok-Req-Id", ids.ReqID)
	}
	if ids.SessionID != "" {
		h.Set("X-Grok-Session-Id", ids.SessionID)
	}
	if ids.AgentID != "" {
		h.Set("X-Grok-Agent-Id", ids.AgentID)
	}
	// Group id is optional in the official sampler and stays unset when the
	// caller has no separate conversation root. Reusing the session hash would
	// invent a root the CLI does not invent.
	if turn := countUserTurns(body); turn > 0 {
		h.Set("X-Grok-Turn-Idx", fmt.Sprintf("%d", turn))
	}
}

// countUserTurns counts Responses user message items in the encoded body.
// It is not a process-wide turn counter. A missing input array yields 0 and
// the header is omitted.
func countUserTurns(body []byte) int {
	var doc struct {
		Input []struct {
			Type string `json:"type"`
			Role string `json:"role"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return 0
	}
	n := 0
	for _, item := range doc.Input {
		if item.Role == "user" && (item.Type == "" || item.Type == "message") {
			n++
		}
	}
	return n
}

// Model is one selectable catalog entry. Effort lists are present only when
// the account payload included them. They are not a local whitelist.
type Model struct {
	ID      string
	Name    string
	Efforts []string
}

// ParseModels reads an OpenAI-style /v1/models document. An empty catalog is an
// error. grok-4.7 is not invented here.
func ParseModels(body []byte) ([]Model, error) {
	var doc struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, errors.New("grok-build: model catalog was not valid JSON")
	}
	seen := map[string]struct{}{}
	var out []Model
	for _, raw := range doc.Data {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		id := firstString(item, "id", "model", "model_id", "modelId")
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		name := firstString(item, "name", "display_name", "displayName")
		if name == "" {
			name = id
		}
		out = append(out, Model{ID: id, Name: name, Efforts: effortList(item)})
	}
	if len(out) == 0 {
		return nil, errors.New("grok-build: model catalog returned no selectable models")
	}
	return out, nil
}

func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := item[key].(string); ok {
			s = strings.TrimSpace(s)
			if s != "" {
				return s
			}
		}
	}
	return ""
}

func effortList(item map[string]any) []string {
	caps, _ := item["capabilities"].(map[string]any)
	if caps == nil {
		return nil
	}
	raw, _ := caps["reasoning_effort"].([]any)
	var out []string
	seen := map[string]struct{}{}
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if !recognizedEffort(s) {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func recognizedEffort(s string) bool {
	switch s {
	case "none", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

// ListModels fetches the account catalog. The token is attached only after the
// URL is checked. Redirects are not followed. accountUserID is the token
// principal. An empty value omits x-userid rather than inventing one.
func ListModels(ctx context.Context, accessToken, accountUserID string) ([]Model, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("grok-build: access token is missing")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(TokenAuthHeader, TokenAuthValue)
	req.Header.Set("X-Grok-Client-Version", Version)
	req.Header.Set("X-Grok-Client-Mode", ClientMode)
	req.Header.Set("User-Agent", UserAgent())
	if userID := NormalizeIdentity(accountUserID); userID != "" {
		req.Header.Set("X-Userid", userID)
	}
	resp, err := DoFixed(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("grok-build: model catalog read failed")
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		return nil, VersionMismatchError()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("grok-build: model discovery failed (HTTP %d)", resp.StatusCode)
	}
	return ParseModels(body)
}

var (
	transportMu   sync.RWMutex
	testTransport http.RoundTripper
)

// SetTestTransport installs a RoundTripper for fixed-host auth and catalog
// calls. It does not change the request URL. Production leaves it nil.
func SetTestTransport(rt http.RoundTripper) {
	transportMu.Lock()
	testTransport = rt
	transportMu.Unlock()
}

// ClientNoRedirect copies base and refuses redirects so a subscription bearer
// is not forwarded to another path or host. A nil base returns nil.
func ClientNoRedirect(base *http.Client) *http.Client {
	if base == nil {
		return nil
	}
	clone := *base
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("grok-build: redirect refused")
	}
	return &clone
}

// DoFixed sends req only when its URL is a fixed auth or inference URL. The
// client does not follow redirects. Billing is not part of this allowlist.
func DoFixed(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errUnsafeHost
	}
	raw := req.URL.String()
	if !IsFixedDeviceURL(raw) && !IsFixedTokenURL(raw) && !IsFixedInferenceURL(raw) {
		return nil, errUnsafeHost
	}
	return doNoRedirect(ctx, req)
}

func fixedClient() *http.Client {
	transportMu.RLock()
	rt := testTransport
	transportMu.RUnlock()
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectRefused
		},
	}
}

func doNoRedirect(ctx context.Context, req *http.Request) (*http.Response, error) {
	if ctx != nil && req != nil {
		req = req.WithContext(ctx)
	}
	return fixedClient().Do(req)
}

// isExactBillingURL accepts only the fixed credits URL, including the
// format=credits query. Any other query, userinfo, fragment, or path fails.
// This check is not the inference allowlist.
func isExactBillingURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), inferenceHost) {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	if u.EscapedPath() != "/v1/billing" || u.RawQuery != "format=credits" {
		return false
	}
	return true
}

// FetchBilling reads the fixed credits billing endpoint. The URL and method
// are not taken from a provider, a form, or a caller argument. accountUserID
// is sent as x-userid only after NormalizeIdentity. An empty result omits the
// header. A returned error does not include the upstream body, a redirect
// location, or the credential. The body is returned only for a 2xx response
// that fits in billingBodyLimit.
func FetchBilling(ctx context.Context, accessToken, accountUserID string) (int, []byte, error) {
	if strings.TrimSpace(accessToken) == "" {
		return 0, nil, errors.New("grok-build: access token is missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BillingURL, nil)
	if err != nil || req.URL == nil || req.Method != http.MethodGet || !isExactBillingURL(req.URL.String()) {
		return 0, nil, errBillingTransport
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(TokenAuthHeader, TokenAuthValue)
	req.Header.Set("X-Grok-Client-Version", Version)
	req.Header.Set("X-Grok-Client-Mode", ClientMode)
	req.Header.Set("User-Agent", UserAgent())
	if userID := NormalizeIdentity(accountUserID); userID != "" {
		req.Header.Set("X-Userid", userID)
	}
	resp, err := doNoRedirect(ctx, req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if errors.Is(err, errRedirectRefused) {
			return 0, nil, errBillingRedirect
		}
		return 0, nil, errBillingTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return 0, nil, errBillingRedirect
	}
	body, err := readBounded(resp.Body, billingBodyLimit)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		return resp.StatusCode, nil, VersionMismatchError()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, body, nil
}

// readBounded rejects a body larger than limit instead of parsing a prefix.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errBillingRead
	}
	if int64(len(data)) > limit {
		return nil, errBillingOversize
	}
	return data, nil
}
