package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/harlog"
	"airouter/internal/oauth"
	"airouter/internal/observability"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/qoder"
	"airouter/internal/proxy/responses"
)

const anthropicVersion = "2023-06-01"

// applyCodexHeaders sets the Codex-CLI identity headers the ChatGPT backend
// requires: User-Agent, originator, session_id (the per-request CodexSessionID
// carried on the trace context), and chatgpt-account-id (from the id_token when
// the connection extracted one). session_id is also used as prompt_cache_key.
func applyCodexHeaders(req *http.Request, provider *domain.Provider, ctx context.Context) {
	req.Header.Set("User-Agent", "codex_cli_rs/"+responses.CodexCLIVersion)
	req.Header.Set("originator", "codex_cli_rs")
	if t := traceInfoFrom(ctx); t != nil && t.CodexSessionID != "" {
		req.Header.Set("session_id", t.CodexSessionID)
	}
	if provider.OAuthCreds != nil && provider.OAuthCreds.AccountID != "" {
		req.Header.Set("chatgpt-account-id", provider.OAuthCreds.AccountID)
	}
}

// newCodexSessionID returns a random id suitable for the Codex session_id header
// and prompt_cache_key. Anthropic-style UUIDs are not required here; a hex token
// is enough and avoids the format's hyphens in a header value.
func newCodexSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// prepareUpstreamRequest applies backend-specific post-encode patches to the
// upstream body that need per-request state or provider config the codec's
// encodeRequest cannot see:
//
//   - Codex: sets up the per-request session id and injects it as
//     prompt_cache_key; the id is saved on the trace context so applyCodexHeaders
//     emits it as the session_id header.
//   - Kiro: injects the provider's CodeWhisperer profile ARN into the request.
//   - Qoder: injects live model_config and WAF-encodes the body (COSY signs
//     these wire bytes in applyUpstreamHeaders).
//   - Antigravity: injects OAuthCreds.ProjectID into the Cloud Code envelope
//     (fail-closed when missing).
//   - Claude Code: generates the per-request session id (saved on the trace
//     context for the X-Claude-Code-Session-Id header) and applies the OAuth-only
//     cloak/decoy transform. The cloak gate and seed read OAuthCreds directly
//     because the access token is resolved later, inside forward/forwardStream.
//
// Other backends return the body unchanged. A non-nil error is terminal for the
// attempt (e.g. Qoder model_config unknown).
func prepareUpstreamRequest(ctx context.Context, backend codec, provider *domain.Provider, body []byte) ([]byte, error) {
	switch backend.id {
	case "oai-codex":
		id := newCodexSessionID()
		if t := traceInfoFrom(ctx); t != nil {
			t.CodexSessionID = id
		}
		return responses.InjectCodexRequestKey(body, id), nil
	case "kiro":
		return kiro.InjectProfileArn(body, kiroProfileArn(provider)), nil
	case "qoder":
		wire, err := qoder.PrepareWireBody(ctx, provider, body)
		if err != nil {
			return nil, err
		}
		// Stash model key/source from plaintext before encode for header emission.
		// PrepareWireBody already encoded; re-read key from original body.
		if t := traceInfoFrom(ctx); t != nil {
			t.QoderModelKey = qoder.ModelKeyFromBody(body)
			if cfg, lerr := qoder.LookupModelConfig(ctx, provider, t.QoderModelKey); lerr == nil {
				t.QoderModelSource = qoder.ModelSourceFromConfig(cfg)
			}
			if t.QoderModelSource == "" {
				t.QoderModelSource = "system"
			}
		}
		return wire, nil
	case "antigravity":
		return antigravity.InjectProjectID(body, antigravityProjectID(provider))
	case "claude-code":
		sid := newUUID()
		if t := traceInfoFrom(ctx); t != nil {
			t.ClaudeCodeSessionID = sid
		}
		return claudecode.ApplyOAuthCloaking(body, claudeCodeToken(provider), sid, claudeCodeSeed(provider))
	case "cursor":
		// Cursor needs no body mutation; headers (checksum, identity) are applied
		// in applyUpstreamHeaders after the client-header copy.
		return body, nil
	default:
		return body, nil
	}
}

// antigravityProjectID returns the Cloud Code project id from OAuthCreds.
func antigravityProjectID(provider *domain.Provider) string {
	if provider != nil && provider.OAuthCreds != nil {
		return provider.OAuthCreds.ProjectID
	}
	return ""
}

// claudeCodeToken returns the stored Claude OAuth access token, used only for the
// sk-ant-oat cloak gate. The actual Authorization header uses the resolved
// (refreshed) token set by forward; the stored token's marker is stable across
// refresh, so reading it at prepare time (before resolve) is correct.
func claudeCodeToken(provider *domain.Provider) string {
	if provider != nil && provider.OAuthCreds != nil {
		return provider.OAuthCreds.AccessToken
	}
	return ""
}

// claudeCodeSeed returns a stable per-account seed for metadata.user_id,
// preferring the refresh token (stable across access-token refresh) and falling
// back to the access token. Empty when unset, in which case the cloak helper
// generates random device/account values.
func claudeCodeSeed(provider *domain.Provider) string {
	if provider == nil || provider.OAuthCreds == nil {
		return ""
	}
	if s := strings.TrimSpace(provider.OAuthCreds.RefreshToken); s != "" {
		return s
	}
	return provider.OAuthCreds.AccessToken
}

// kiroProfileArn returns the CodeWhisperer profile ARN configured for a Kiro
// provider, from its OAuthCreds (which carries the field for both apikey and
// oauth Kiro providers). Empty when unset.
func kiroProfileArn(provider *domain.Provider) string {
	if provider != nil && provider.OAuthCreds != nil {
		return provider.OAuthCreds.ProfileArn
	}
	return ""
}

// hopByHopOrControlled are request headers we never copy from the client: either
// the transport owns them, or we set them ourselves (auth). Dropping the client
// auth headers lets us substitute the provider's credential.
var hopByHopOrControlled = map[string]bool{
	"Host":              true,
	"Content-Length":    true,
	"Connection":        true,
	"Accept-Encoding":   true,
	"Authorization":     true,
	"X-Api-Key":         true,
	"Keep-Alive":        true,
	"Proxy-Connection":  true,
	"Transfer-Encoding": true,
}

// applyUpstreamHeaders copies the client's request headers onto the upstream
// request (under the denylist above), then sets the provider auth. Forwarding
// client headers preserves caller identity (User-Agent, x-app, anthropic-beta,
// x-stainless-*), which some providers require: an Anthropic upstream may reject
// a request that does not look like it came from the official client. ctx is the
// per-request context so codex headers (session_id) can read the trace session id.
// body is the exact POST body that will be sent; Qoder COSY signing hashes it.
// Non-Qoder backends ignore body.
func applyUpstreamHeaders(req *http.Request, provider *domain.Provider, clientHeaders http.Header, ctx context.Context, body []byte) {
	for name, vals := range clientHeaders {
		if hopByHopOrControlled[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// Credential header depends on the auth scheme, which is independent of the
	// protocol: an Anthropic-format provider may use a bearer token.
	switch provider.Auth() {
	case domain.AuthXAPIKey:
		req.Header.Set("x-api-key", provider.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	// anthropic-version is a wire-format requirement of the Anthropic Messages
	// API, tied to protocol rather than auth. Preserve a client-sent value.
	if provider.Protocol == domain.ProtocolAnthropic && req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", anthropicVersion)
	}
	// The Codex backend additionally requires the Codex-CLI identity headers.
	// User-Agent is set after the client-header copy so it overrides any
	// forwarded client User-Agent the Codex backend would reject.
	if provider.Protocol == domain.ProtocolOpenAICodex {
		applyCodexHeaders(req, provider, ctx)
	}
	// Kiro requires the CodeWhisperer/AWS-SDK identity headers; a malformed
	// User-Agent is rejected upstream. Set after the client-header copy so they
	// override any forwarded values.
	if provider.Protocol == domain.ProtocolKiro {
		applyKiroHeaders(req, provider)
	}
	// Cline/ClinePass OAuth needs the workos: bearer prefix and Cline identity
	// headers; set after the auth-scheme switch so Authorization is rewritten.
	if provider.OAuthCreds != nil && provider.OAuthCreds.ClineAuth {
		applyClineHeaders(req, provider)
	}
	// Qoder overwrites Authorization with COSY and sets identity / model headers.
	// Must run last so Cosy-* and Accept-Encoding: identity win.
	if provider.Protocol == domain.ProtocolQoder {
		applyQoderHeaders(req, provider, ctx, body)
	}
	// Antigravity forces the IDE User-Agent after the client-header copy.
	if provider.Protocol == domain.ProtocolAntigravity {
		req.Header.Set("User-Agent", antigravity.UserAgent)
	}
	// Cursor overwrites the full identity header set (Content-Type, checksum,
	// x-cursor-* identity) after the client-header copy so forwarded values the
	// Cursor backend would reject do not leak through.
	if provider.Protocol == domain.ProtocolCursor {
		applyCursorHeaders(req, provider)
	}
	// Claude Code overwrites the CLI fingerprint (anthropic-version/beta,
	// User-Agent, X-Stainless-*) and sets X-Claude-Code-Session-Id after the
	// client-header copy so a forwarded client identity the Anthropic backend
	// would reject does not leak through. Auth is already set by the scheme
	// switch above; this does not touch Authorization.
	if provider.Protocol == domain.ProtocolClaudeCode {
		applyClaudeCodeHeaders(req, ctx)
	}
}

// applyQoderHeaders COSY-signs the wire body and sets Qoder identity headers.
func applyQoderHeaders(req *http.Request, provider *domain.Provider, ctx context.Context, body []byte) {
	reqURL := req.URL.String()
	if !req.URL.IsAbs() {
		// NewRequest with absolute URL sets scheme/host; prefer that form.
		reqURL = req.URL.RequestURI()
		if req.URL.Scheme != "" && req.URL.Host != "" {
			reqURL = req.URL.Scheme + "://" + req.URL.Host + req.URL.RequestURI()
		}
	}
	headers, err := qoder.BuildCosyHeaders(body, reqURL, qoder.CredsFromProvider(provider))
	if err != nil {
		// Leave prior Authorization; upstream will 401 and surface reconnect.
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if t := traceInfoFrom(ctx); t != nil {
		if t.QoderModelKey != "" {
			req.Header.Set("X-Model-Key", t.QoderModelKey)
		}
		src := t.QoderModelSource
		if src == "" {
			src = "system"
		}
		req.Header.Set("X-Model-Source", src)
	}
}

// applyClineHeaders sets Cline identity headers and normalizes the bearer token
// with the workos: prefix the upstream requires. Auth method is already bearer
// from the scheme switch; this overwrites Authorization with the prefixed form.
func applyClineHeaders(req *http.Request, provider *domain.Provider) {
	for k, v := range oauth.ClineIdentityHeaders("", provider.APIKey) {
		req.Header.Set(k, v)
	}
}

// applyCursorHeaders sets the Cursor Connect-RPC identity headers (checksum,
// x-cursor-* fingerprint, Content-Type) overwriting any forwarded client
// values. The bearer token has any "::" prefix stripped; the machine id comes
// from OAuthCreds, falling back to a token-derived hash.
func applyCursorHeaders(req *http.Request, provider *domain.Provider) {
	token := strings.TrimSpace(provider.APIKey)
	machineID := ""
	if provider.OAuthCreds != nil {
		machineID = strings.TrimSpace(provider.OAuthCreds.MachineID)
	}
	h := cursor.BuildHeaders(token, machineID, true)
	for k, vv := range h {
		for _, v := range vv {
			req.Header.Set(k, v)
		}
	}
}

// applyKiroHeaders sets the CodeWhisperer identity headers and, for an apikey
// provider, the tokentype marker the upstream keys host acceptance on. The
// Amz-Sdk-Invocation-Id is a fresh uuid per request. Authorization is already
// set to the bearer credential by the auth-scheme switch above.
func applyKiroHeaders(req *http.Request, provider *domain.Provider) {
	req.Header.Set("X-Amz-Target", kiro.XAmzTarget)
	req.Header.Set("User-Agent", kiro.UserAgent)
	req.Header.Set("X-Amz-User-Agent", kiro.XAmzUserAgent)
	req.Header.Set("Amz-Sdk-Request", kiro.AmzSdkRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", newUUID())
	if provider.Method() == domain.AuthAPIKey {
		req.Header.Set("tokentype", "API_KEY")
	}
}

// applyClaudeCodeHeaders sets the Claude Code CLI identity fingerprint and the
// per-request X-Claude-Code-Session-Id (which must match metadata.user_id.session_id
// in the body). Runs after the client-header copy so it overwrites forwarded
// values. Authorization is left to the auth-scheme switch.
func applyClaudeCodeHeaders(req *http.Request, ctx context.Context) {
	for k, v := range claudecode.IdentityHeaders() {
		req.Header.Set(k, v)
	}
	if t := traceInfoFrom(ctx); t != nil && t.ClaudeCodeSessionID != "" {
		req.Header.Set(claudecode.SessionIDHeader, t.ClaudeCodeSessionID)
	}
}

// newUUID returns a random RFC 4122 version-4 UUID string for the
// Amz-Sdk-Invocation-Id header and the Claude Code session id.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// forward sends the prepared body to the provider's upstream endpoint for the
// given backend protocol, setting the protocol-appropriate auth headers.
// clientHeaders, when non-nil (passthrough), are forwarded under the denylist.
//
// For oauth providers the access token is resolved (and proactively refreshed
// when near expiry) before the first send; on a 401/403 the token is forcibly
// refreshed and the request retried once.
func (p *Proxy) forward(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header) (int, []byte, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	send := func() (int, []byte, error) {
		started := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders, ctx, body)
		// Snapshot headers after auth/identity so HAR sees the wire credentials.
		var harHdr http.Header
		if harRecorder(ctx) != nil {
			harHdr = req.Header.Clone()
		}
		resp, err := p.client.Do(req)
		if err != nil {
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, body, 0, nil, nil, "", 0, err.Error())
			}
			return 0, nil, err
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, body, resp.StatusCode, resp.Header, respBody, "", len(respBody), err.Error())
			}
			return resp.StatusCode, nil, err
		}
		if harRecorder(ctx) != nil {
			p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, body, resp.StatusCode, resp.Header, respBody, "", len(respBody), "")
		}
		return resp.StatusCode, respBody, nil
	}

	if err := p.resolveToken(ctx, provider, false); err != nil {
		// Proactive refresh failed; proceed with the existing token (it may still
		// work, or the reactive path below will catch the 401).
		observability.Logger(ctx, p.logger).Debug("oauth_resolve_failed",
			"event", "oauth_resolve_failed",
			"provider", provider.Name,
			"error", "OAuth token resolution failed",
		)
	}
	status, respBody, err := send()
	if err != nil {
		return status, respBody, err
	}
	if isAuthFailure(status) && provider.Method() == domain.AuthOAuth {
		if rerr := p.resolveToken(ctx, provider, true); rerr == nil {
			return send()
		} else {
			observability.Logger(ctx, p.logger).Debug("oauth_forced_refresh_failed",
				"event", "oauth_forced_refresh_failed",
				"provider", provider.Name,
				"status", status,
				"reconnect_required", oauth.IsInvalidGrant(rerr),
				"error", "OAuth forced refresh failed",
			)
		}
	}
	return status, respBody, nil
}

// forwardStream sends the body and returns the live response for streaming.
// The caller owns closing resp.Body. Used for SSE responses. Token resolution
// and the reactive 401/403 retry mirror forward, but must complete before the
// stream is handed back since the status is only inspected once.
func (p *Proxy) forwardStream(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header, streamAccept string) (*http.Response, error) {
	url := strings.TrimRight(provider.BaseURL, "/") + path
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	accept := streamAccept
	if accept == "" {
		accept = "text/event-stream"
	}
	send := func() (*http.Response, error) {
		started := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders, ctx, body)
		req.Header.Set("Accept", accept)
		var harHdr http.Header
		if harRecorder(ctx) != nil {
			harHdr = req.Header.Clone()
		}
		resp, err := p.streamClient.Do(req)
		if err != nil {
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, body, 0, nil, nil, "", 0, err.Error())
			}
			return nil, err
		}
		if harRecorder(ctx) != nil {
			// Wrap so Close finalizes the upstream HAR entry (including the
			// 401 body that is closed before an OAuth retry).
			resp.Body = &harCaptureBody{
				rc:      resp.Body,
				started: started,
				method:  req.Method,
				url:     url,
				reqHdr:  harHdr,
				reqBody: body,
				status:  resp.StatusCode,
				respHdr: resp.Header.Clone(),
				mime:    streamRespMIME(resp, accept),
				record:  p.recordUpstreamHAR,
				ctx:     ctx,
			}
		}
		return resp, nil
	}

	if err := p.resolveToken(ctx, provider, false); err != nil {
		observability.Logger(ctx, p.logger).Debug("oauth_resolve_failed",
			"event", "oauth_resolve_failed",
			"provider", provider.Name,
			"error", "OAuth token resolution failed",
		)
	}
	resp, err := send()
	if err != nil {
		return resp, err
	}
	if isAuthFailure(resp.StatusCode) && provider.Method() == domain.AuthOAuth {
		if rerr := p.resolveToken(ctx, provider, true); rerr == nil {
			resp.Body.Close()
			return send()
		} else {
			observability.Logger(ctx, p.logger).Debug("oauth_forced_refresh_failed",
				"event", "oauth_forced_refresh_failed",
				"provider", provider.Name,
				"status", resp.StatusCode,
				"reconnect_required", oauth.IsInvalidGrant(rerr),
				"error", "OAuth forced refresh failed",
			)
		}
	}
	return resp, nil
}

// harRecorder returns the request-pinned HAR recorder, or nil when capture is off.
func harRecorder(ctx context.Context) *harlog.Recorder {
	if t := traceInfoFrom(ctx); t != nil {
		return t.HAR
	}
	return nil
}

// recordUpstreamHAR writes one upstream leg into the request-pinned HAR
// recorder. pageID comes from TraceInfo.RequestID so it shares a page with the
// ingress entry. respBodySize is the full wire length (may exceed len(respBody)
// when a stream tee already truncated).
func (p *Proxy) recordUpstreamHAR(ctx context.Context, started time.Time, dur time.Duration, method, url string, reqHdr http.Header, reqBody []byte, status int, respHdr http.Header, respBody []byte, respMIME string, respBodySize int, failure string) {
	rec := harRecorder(ctx)
	if rec == nil {
		return
	}
	pageID := ""
	if t := traceInfoFrom(ctx); t != nil && t.RequestID != "" {
		pageID = "page_" + t.RequestID
	}
	if pageID == "" {
		return
	}
	title := method + " " + url
	rec.EnsurePage(pageID, title, started)
	rec.Record(harlog.RecordInput{
		PageID:       pageID,
		StartedAt:    started,
		Duration:     dur,
		Method:       method,
		URL:          url,
		ReqHeaders:   reqHdr,
		ReqBody:      reqBody,
		Status:       status,
		RespHeaders:  respHdr,
		RespBody:     respBody,
		RespMIME:     respMIME,
		RespBodySize: respBodySize,
		Failure:      failure,
	})
}

func streamRespMIME(resp *http.Response, accept string) string {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	if strings.Contains(strings.ToLower(accept), "event-stream") {
		return "text/event-stream"
	}
	return accept
}

// harCaptureBody tees a bounded copy of a streaming upstream body and records
// the HAR entry on Close. Read bytes are relayed unchanged; the tee uses
// observability.Capture capped at harlog.MaxBody.
type harCaptureBody struct {
	rc      io.ReadCloser
	started time.Time
	method  string
	url     string
	reqHdr  http.Header
	reqBody []byte
	status  int
	respHdr http.Header
	mime    string
	record  func(ctx context.Context, started time.Time, dur time.Duration, method, url string, reqHdr http.Header, reqBody []byte, status int, respHdr http.Header, respBody []byte, respMIME string, respBodySize int, failure string)
	ctx     context.Context

	mu      sync.Mutex
	cap     *observability.Capture
	readErr error
	closed  bool
}

func (c *harCaptureBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.mu.Lock()
	if n > 0 {
		if c.cap == nil {
			c.cap = observability.NewCapture(harlog.MaxBody)
		}
		_, _ = c.cap.Write(p[:n])
	}
	if err != nil && err != io.EOF {
		c.readErr = err
	}
	c.mu.Unlock()
	return n, err
}

func (c *harCaptureBody) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.rc.Close()
	}
	c.closed = true
	var body []byte
	var total int
	if c.cap != nil {
		body = append([]byte(nil), c.cap.Bytes()...)
		total = int(c.cap.Total())
	}
	var failure string
	if c.readErr != nil {
		failure = c.readErr.Error()
	} else if err := c.ctx.Err(); err != nil {
		failure = err.Error()
	}
	c.mu.Unlock()

	// If nothing was read (e.g. closed after a 401 before the body was drained),
	// still record headers/status with an empty body. total may exceed len(body)
	// when the tee hit MaxBody.
	if c.record != nil {
		c.record(c.ctx, c.started, time.Since(c.started), c.method, c.url, c.reqHdr, c.reqBody, c.status, c.respHdr, body, c.mime, total, failure)
	}
	return c.rc.Close()
}

// resolveToken sets provider.APIKey to the effective upstream credential. For
// oauth providers it resolves (and may refresh) the access token; for apikey
// providers Resolve returns the static key unchanged, so this is a no-op. The
// provider is the request-local hydrated copy, so the mutation is request-scoped.
// On error the best-available token is still written, so callers may proceed.
func (p *Proxy) resolveToken(ctx context.Context, provider *domain.Provider, force bool) error {
	tok, err := p.oauth.Resolve(ctx, provider, force)
	provider.APIKey = tok
	return err
}

// isAuthFailure reports whether an upstream status indicates a rejected
// credential, the trigger for a reactive OAuth token refresh.
func isAuthFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}
