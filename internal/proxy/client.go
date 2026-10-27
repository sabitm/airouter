package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/harlog"
	"airouter/internal/kirocatalog"
	"airouter/internal/oauth"
	"airouter/internal/observability"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/qoder"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/thinking"
)

const (
	anthropicVersion                    = "2023-06-01"
	maxUnaryUpstreamResponseBytes int64 = 64 << 20
)

// applyCodexHeaders sets the Codex-CLI identity headers the ChatGPT backend
// requires: User-Agent, originator, session_id (the derived CodexSessionID
// carried on the trace context), and chatgpt-account-id (from the id_token when
// the connection extracted one). session_id must equal prompt_cache_key.
func applyCodexHeaders(req *http.Request, provider *domain.Provider, ctx context.Context) {
	req.Header.Set("User-Agent", "codex_cli_rs/"+responses.CodexCLIVersion)
	req.Header.Set("originator", "codex_cli_rs")
	if id := codexSessionID(ctx); id != "" {
		req.Header.Set("session_id", id)
	}
	if provider.OAuthCreds != nil && provider.OAuthCreds.AccountID != "" {
		req.Header.Set("chatgpt-account-id", provider.OAuthCreds.AccountID)
	}
}

// prepareUpstreamRequest applies backend-specific post-encode patches to the
// upstream body that need per-request state or provider config the codec's
// encodeRequest cannot see:
//
//   - Codex: derives a tenant+provider-scoped cache key from captured client
//     identity (or a conversation-prefix / request-local fallback) and injects
//     it as prompt_cache_key; the same value is saved on the trace context so
//     applyCodexHeaders emits it as the session_id header.
//   - Kiro: injects the provider's CodeWhisperer profile ARN into the request.
//   - Qoder: injects live model_config and WAF-encodes the body (COSY signs
//     these wire bytes in applyUpstreamHeaders).
//   - Antigravity: injects OAuthCreds.ProjectID into the Cloud Code envelope
//     (fail-closed when missing).
//   - Claude Code: defers session and body preparation to forward, after the
//     effective token is resolved, so billing and metadata use that token and
//     the current persisted account UUID.
//   - OpenCode: resolves x-opencode-session (client identity, else a fallback
//     namespaced by this Proxy's nonce and the provider) onto the trace context
//     and applies the model-specific patch: muse-spark Responses normalization
//     or the Chat reasoning_content echo for kimi/deepseek.
//
// Other backends return the body unchanged. A non-nil error is terminal for the
// attempt (e.g. Qoder model_config unknown).
func (p *Proxy) prepareUpstreamRequest(ctx context.Context, backend codec, provider *domain.Provider, body []byte) ([]byte, error) {
	return p.prepareUpstreamRequestFor(ctx, backend, provider, nil, body)
}

func (p *Proxy) prepareUpstreamRequestFor(ctx context.Context, backend codec, provider *domain.Provider, req *ir.Request, body []byte) ([]byte, error) {
	if p != nil && isOpencodeCodec(backend.id) {
		ctx = withOpencodeNonce(ctx, p.opencodeNonce)
		ctx = withOpencodeSessions(ctx, p.opencodeSessions)
	}
	return prepareUpstreamRequest(ctx, p, backend, provider, req, body)
}

func isOpencodeCodec(id string) bool {
	switch id {
	case "opencode-chat", "opencode-responses", "opencode-messages":
		return true
	default:
		return false
	}
}

func prepareUpstreamRequest(ctx context.Context, p *Proxy, backend codec, provider *domain.Provider, req *ir.Request, body []byte) ([]byte, error) {
	switch backend.id {
	case "oai-codex":
		id := resolveCodexCacheKey(ctx, provider)
		body, err := responses.InjectCodexRequestKey(body, id)
		if err != nil {
			return nil, fmt.Errorf("codex prompt_cache_key: %w", err)
		}
		if t := traceInfoFrom(ctx); t != nil {
			t.CodexSessionID = id
		}
		return body, nil
	case "kiro":
		body, err := p.applyKiroCapability(ctx, provider, req, body)
		if err != nil {
			return nil, err
		}
		return kiro.InjectProfileArn(body, kiro.ProfileArnForBody(kiro.IdentityFromProvider(provider))), nil
	case "kiro-runtime":
		body, err := p.applyKiroCapability(ctx, provider, req, body)
		if err != nil {
			return nil, err
		}
		id := kiro.IdentityFromProvider(provider)
		body = kiro.InjectProfileArn(body, kiro.ProfileArnForBody(id))
		return kiro.InjectRuntimeAgentMode(body, id.AgentMode), nil
	case "qoder":
		requested := 0
		var wantThinking *ir.Thinking
		if req != nil {
			requested = req.MaxTokens
			wantThinking = req.Thinking
		}
		prep, err := qoder.PrepareWireBodyWithThinking(ctx, provider, requested, wantThinking, body)
		if err != nil {
			return nil, err
		}
		// Identity and model headers ride the trace: the encoded wire bytes
		// cannot be re-read for them, and queue recovery needs the same values.
		if t := traceInfoFrom(ctx); t != nil {
			t.QoderModelKey = prep.ModelKey
			t.QoderModelSource = prep.ModelSource
			t.QoderRequestID = prep.RequestID
			t.QoderRequestSetID = prep.RequestSetID
			t.QoderSessionID = prep.SessionID
			if t.QoderModelSource == "" {
				t.QoderModelSource = "system"
			}
		}
		return prep.Wire, nil
	case "antigravity":
		return antigravity.InjectProjectID(body, antigravityProjectID(provider))
	case "claude-code":
		// Preparation needs the effective credential snapshot. Defer the session
		// and body until forward, after Resolve, so a refresh cannot leave a stale
		// account scope on the trace or in the wire body.
		return body, nil
	case "opencode-chat", "opencode-responses", "opencode-messages":
		prepareOpencodeSession(ctx, provider, body)
		body, err := prepareOpencodeFreeTier(ctx, backend, provider, body)
		if err != nil {
			return nil, err
		}
		if backend.id == "opencode-chat" {
			return opencode.InjectReasoningEcho(body, reqModelFromBody(body))
		}
		if backend.id == "opencode-responses" {
			return opencode.PrepareMuseSparkResponse(body)
		}
		return body, nil
	case "cursor":
		// Cursor needs no body mutation; headers (checksum, identity) are applied
		// in applyUpstreamHeaders after the client-header copy.
		return body, nil
	default:
		return body, nil
	}
}

// applyKiroCapability resolves the live model schema and patches the encoded
// envelope. Discovery uses the effective token before catalog HTTP. Catalog
// failure omits controls and does not fail the attempt. A canceled caller
// context stops before the chat send. The function does not mutate req or the
// shared provider.
func (p *Proxy) applyKiroCapability(ctx context.Context, provider *domain.Provider, req *ir.Request, body []byte) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if p == nil || p.kiroCatalog == nil || provider == nil || req == nil {
		return body, nil
	}
	catalogProvider := kiroCatalogProvider(provider)
	var refresh func(context.Context) (string, error)
	if catalogProvider.Method() == domain.AuthOAuth && p.oauth != nil {
		tok, err := p.resolveKiroCatalogToken(ctx, catalogProvider, false)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			observability.Logger(ctx, p.logger).Debug("kiro_catalog_resolve_failed",
				"event", "kiro_catalog_resolve_failed",
				"provider", provider.Name,
				"reason", "oauth_resolve_failed",
			)
			return body, nil
		}
		catalogProvider.APIKey = tok
		refresh = func(refreshCtx context.Context) (string, error) {
			forced, rerr := p.resolveKiroCatalogToken(refreshCtx, catalogProvider, true)
			if rerr != nil {
				observability.Logger(refreshCtx, p.logger).Debug("kiro_catalog_refresh_failed",
					"event", "kiro_catalog_refresh_failed",
					"provider", provider.Name,
					"reason", "oauth_refresh_failed",
				)
			}
			return forced, rerr
		}
	}
	lookup, err := p.kiroCatalog.LookupWithRefresh(ctx, p.logger, catalogProvider, req.Model, refresh)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return body, nil
	}
	effort, effortSet, thinkingType, budget := kiroIntent(req.Thinking)
	sel := kiro.ResolveSelection(lookup.Capability, effort, effortSet, thinkingType, budget)
	if sel != nil && sel.BudgetIgnored {
		observability.Logger(ctx, p.logger).Debug("kiro_budget_not_applied",
			"event", "kiro_budget_not_applied",
			"reason", "schema_has_no_budget",
		)
	}
	if sel != nil && sel.EffortOmitted {
		observability.Logger(ctx, p.logger).Debug("kiro_effort_omitted",
			"event", "kiro_effort_omitted",
			"reason", "filtered_enum_empty",
		)
	}
	return kiro.ApplyAdditionalFields(body, sel), nil
}

func kiroPrepareCanceled(ctx context.Context, backend codec, err error) bool {
	if err == nil || ctx == nil || ctx.Err() == nil {
		return false
	}
	return backend.id == "kiro" || backend.id == "kiro-runtime"
}

func kiroCatalogProvider(p *domain.Provider) *domain.Provider {
	if p == nil {
		return nil
	}
	out := *p
	if p.OAuthCreds != nil {
		creds := *p.OAuthCreds
		out.OAuthCreds = &creds
	}
	if p.Tags != nil {
		out.Tags = append([]string(nil), p.Tags...)
	}
	return &out
}

// Resolve can read newer stored credentials. Validate both sides of that read
// so discovery never combines a new account token with an old profile/config.
func (p *Proxy) resolveKiroCatalogToken(ctx context.Context, probe *domain.Provider, force bool) (string, error) {
	return kirocatalog.ResolveToken(ctx, p.oauth, p.store, probe, force)
}

func kiroIntent(t *ir.Thinking) (effort string, effortSet bool, thinkingType string, budget bool) {
	cfg := thinking.FromIR(t)
	if cfg == nil {
		return "", false, "", false
	}
	if cfg.Effort == "none" || cfg.Effort == "off" {
		thinkingType = kiro.ThinkingDisabled
	}
	switch cfg.Enable {
	case thinking.EnableDisabled:
		thinkingType = kiro.ThinkingDisabled
	case thinking.EnableEnabled:
		thinkingType = kiro.ThinkingAdaptive
	}
	budget = cfg.BudgetSet || cfg.Mode == thinking.ModeBudget || cfg.Budget > 0
	if cfg.Effort != "" && cfg.Effort != "none" && cfg.Effort != "off" && cfg.Effort != "auto" {
		return cfg.Effort, true, thinkingType, budget
	}
	switch cfg.Mode {
	case thinking.ModeNone:
		if thinkingType == "" {
			thinkingType = kiro.ThinkingDisabled
		}
		return "", false, thinkingType, budget
	case thinking.ModeAuto:
		return "", false, thinkingType, budget
	case thinking.ModeLevel:
		return cfg.Level, cfg.Level != "", thinkingType, budget
	case thinking.ModeBudget:
		if thinkingType == "" {
			thinkingType = kiro.ThinkingAdaptive
		}
		return "", false, thinkingType, budget
	default:
		return "", false, thinkingType, budget
	}
}

// antigravityProjectID returns the Cloud Code project id from OAuthCreds.
func antigravityProjectID(provider *domain.Provider) string {
	if provider != nil && provider.OAuthCreds != nil {
		return provider.OAuthCreds.ProjectID
	}
	return ""
}

// prepareClaudeCodeBody applies the pinned CLI profile using the effective
// resolved token. Account and device ids come from persisted credentials.
// A missing account stays empty; it is not derived from the token.
func prepareClaudeCodeBody(ctx context.Context, provider *domain.Provider, body []byte) ([]byte, error) {
	// Resolve has already installed the credential snapshot on this request-local
	// provider. Derive the scope from that snapshot every send, not from an
	// earlier attempt's trace or a pre-refresh account.
	sid := claudeSessionID(ctx, provider, claudeTenantFrom(ctx))
	if st := claudeStateFrom(ctx); st != nil {
		st.wireSession = sid
	}
	if t := traceInfoFrom(ctx); t != nil {
		t.ClaudeCodeSessionID = sid
	}
	account, device := claudeIdentity(provider)
	if !claudecode.IsDeviceID(device) {
		if provider != nil && provider.Method() == domain.AuthOAuth {
			return nil, fmt.Errorf("claude-code: device id is not initialized")
		}
		// An API-key backend has no persisted install id. The id is derived from
		// the provider so retries match, and it is not stored as an OAuth device.
		device = syntheticAPIKeyDevice(provider)
	}
	return claudecode.Prepare(body, claudecode.PrepareInput{
		Token:        providerToken(provider),
		SessionID:    sid,
		AccountUUID:  account,
		DeviceID:     device,
		ParentID:     claudeParentID(ctx, provider),
		Profile:      claudeProfileFrom(ctx),
		BaseURL:      providerBase(provider),
		CacheDefault: !claudeCallerCache(body),
		CacheTTL:     "5m",
		Attribution:  claudeAttribution(ctx),
	})
}

// pSessionOrNew keeps the request seed stable while applying the current
// provider scope. It is the request-local seed helper used by older callers.
func pSessionOrNew(ctx context.Context, provider *domain.Provider) string {
	return claudeSessionID(ctx, provider, claudeTenantFrom(ctx))
}

// syntheticAPIKeyDevice derives an install id for an API-key row. The id is
// stable across retries and renames, and it never borrows OAuth identity. Only
// the provider row id and the canonical effective base participate.
func syntheticAPIKeyDevice(p *domain.Provider) string {
	id := int64(0)
	base := ""
	if p != nil {
		id = p.ID
		base = claudeCanonicalBase(effectiveProviderBaseURL(p))
	}
	sum := sha256.Sum256([]byte("claude-apikey-device\x00" + fmt.Sprintf("%d\x00%s", id, base)))
	return hex.EncodeToString(sum[:])
}

// claudeIdentity reads persisted Claude identity only from an explicit OAuth
// row. An API-key row can carry stale or config-only OAuthCreds; those values
// must not become a caller-visible account or install id.
func claudeIdentity(p *domain.Provider) (account, device string) {
	if p == nil || p.Method() != domain.AuthOAuth || p.OAuthCreds == nil {
		return "", ""
	}
	return strings.TrimSpace(p.OAuthCreds.ClaudeAccountUUID), strings.TrimSpace(p.OAuthCreds.ClaudeDeviceID)
}

func claudeAttribution(ctx context.Context) claudecode.BillingInput {
	in := claudecode.BillingInput{PromptID: claudePromptID(ctx)}
	st := claudeStateFrom(ctx)
	if st == nil {
		return in
	}
	if st.agentID != "" {
		in.IsSubagent = true
	}
	return in
}

// claudeWireBody prepares a Claude Code body once per send, after the
// effective token is on the request-local provider. Other protocols return
// the body unchanged. A refresh builds a new body with the new token.
func claudeWireBody(ctx context.Context, provider *domain.Provider, path string, body []byte) ([]byte, error) {
	if provider == nil || provider.Protocol != domain.ProtocolClaudeCode {
		return body, nil
	}
	if !strings.Contains(path, "/messages") {
		return body, nil
	}
	return prepareClaudeCodeBody(ctx, provider, body)
}

func providerToken(p *domain.Provider) string {
	if p == nil {
		return ""
	}
	return p.APIKey
}

func providerBase(p *domain.Provider) string {
	if p == nil {
		return ""
	}
	return p.BaseURL
}

func claudeCallerCache(body []byte) bool {
	// Prepare recounts structured markers, including a top-level policy.
	// A quoted field name in schema or text is not caller policy. This seam
	// therefore does not suppress the default from a substring.
	return false
}

// reqModelFromBody reads the model field of an encoded upstream body; empty
// when absent (the echo patch then stays a no-op).
func reqModelFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
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
	// Kiro owns its operation target. A client-supplied value must not select a
	// different RPC. Content-Type stays copyable for other protocols; Kiro
	// overwrites it after the copy.
	"X-Amz-Target": true,
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
	if (provider.Protocol == domain.ProtocolAnthropic || opencodeMessagesRequest(req, provider)) && req.Header.Get("anthropic-version") == "" {
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
		applyClaudeCodeHeaders(req, ctx, provider, body)
	}
	// OpenCode gates the free tier on the opencode client fingerprint; set it
	// after the client-header copy so a forwarded non-opencode UA is replaced.
	// Authorization keeps the scheme-switch value ("public" or a real key).
	if provider.Protocol == domain.ProtocolOpencode {
		applyOpencodeHeaders(req, provider, ctx, body)
	}
}

func opencodeMessagesRequest(req *http.Request, provider *domain.Provider) bool {
	if provider == nil || provider.Protocol != domain.ProtocolOpencode || req == nil || req.URL == nil {
		return false
	}
	return strings.HasSuffix(req.URL.Path, opencode.MessagesPath)
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

// applyKiroHeaders sets the CodeWhisperer identity headers and the verified
// Kiro auth markers. Authorization is already set by the auth-scheme switch,
// then ApplyAuthHeaders replaces it with the same credential plus identity
// headers that this connection can prove. The invocation id is fresh per request.
func applyKiroHeaders(req *http.Request, provider *domain.Provider) {
	id := kiro.IdentityFromProvider(provider)
	// Drop client-supplied Kiro identity and profile markers before writing the
	// values this connection can prove. The active Runtime serializer does not
	// emit the unused profile HTTP header trait.
	for _, name := range []string{
		"X-Amz-Target",
		"x-amzn-kiro-profile-arn",
		"X-Kiro-Profile-Arn",
		"X-Kiro-Idp",
		"TokenType",
		"x-amzn-kiro-agent-mode",
		"x-amzn-codewhisperer-optout",
	} {
		req.Header.Del(name)
	}
	if kiro.UseRuntime(id) {
		// The active Runtime serializer is AWS JSON 1.0. It writes the Runtime
		// target and does not apply the unused profile HTTP header trait.
		req.Header.Set("Content-Type", kiro.JSONContentType)
		req.Header.Set("X-Amz-Target", kiro.RuntimeTarget)
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Amz-Target", kiro.XAmzTarget)
	}
	req.Header.Set("User-Agent", kiro.UserAgent)
	req.Header.Set("X-Amz-User-Agent", kiro.XAmzUserAgent)
	req.Header.Set("Amz-Sdk-Request", kiro.AmzSdkRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", newUUID())
	kiro.ApplyAuthHeaders(req.Header, id)
}

// applyOpencodeHeaders sets the opencode client fingerprint. Sanitized ingress
// identity (captured before translation) wins for UA/client/project/request/
// session; fingerprinting fills whatever is still missing. Free Zen uses the
// CLI fallback and mirrors the resolved session. Paid Zen, Go, and probes keep
// the desktop defaults and do not synthesize the session-id mirror.
// Authorization stays on the provider credential set by the auth-scheme switch.
func applyOpencodeHeaders(req *http.Request, provider *domain.Provider, ctx context.Context, body []byte) {
	opencode.SanitizeIdentityHeaders(req.Header)
	st := opencodeStateFrom(ctx)
	if st != nil {
		opencode.ApplyIdentity(req.Header, st.identity)
	}
	sid := ""
	if st != nil {
		sid = st.session
	}
	if sid == "" {
		if t := traceInfoFrom(ctx); t != nil {
			sid = t.OpencodeSessionID
		}
	}
	baseURL := ""
	if provider != nil {
		baseURL = provider.BaseURL
	}
	if opencode.NeedsFreeTier(baseURL, reqModelFromBody(body)) {
		// ApplyIdentity maps a generic candidate before this seam. On free Zen the
		// request-state session is the mapped native id and must win. An explicit
		// native client id is already that value. Paid and probe paths keep
		// ApplyIdentity's global mapping.
		if sid != "" {
			req.Header.Set("x-opencode-session", sid)
		}
		// Drop an unvalidated mirror before free fingerprinting. The helper then
		// writes only the session resolved above, so a conflicting copy cannot drift.
		req.Header.Del(opencode.SessionIDHeader)
		opencode.FingerprintFreeHeaders(req.Header, sid)
		return
	}
	opencode.FingerprintHeaders(req.Header, sid)
}

// opencodeReqState is request-private OpenCode identity plus the session
// resolved for the current upstream attempt. Client Session stays on identity;
// session is rewritten per provider so failover does not reuse a fallback.
type opencodeReqState struct {
	identity opencode.Identity
	nonce    string
	session  string
	// sessions is the Proxy that owns this request. Nil keeps deterministic
	// ResolveSession, including direct header helpers without a Proxy.
	sessions *opencode.SessionMap
	// tools is the free-tier rename and decoy map for the current attempt.
	// It is rewritten per provider so a later paid or non-OpenCode target does
	// not inherit a previous attempt's map.
	tools opencode.ToolMap
}

type opencodeStateKeyT struct{}

var opencodeStateKey opencodeStateKeyT

func withOpencodeRequest(ctx context.Context, nonce string, h http.Header, body []byte) context.Context {
	st := &opencodeReqState{
		identity: opencode.CaptureIdentity(h, body),
		nonce:    nonce,
	}
	return context.WithValue(ctx, opencodeStateKey, st)
}

func opencodeStateFrom(ctx context.Context) *opencodeReqState {
	st, _ := ctx.Value(opencodeStateKey).(*opencodeReqState)
	return st
}

func withOpencodeNonce(ctx context.Context, nonce string) context.Context {
	if st := opencodeStateFrom(ctx); st != nil {
		if st.nonce == "" {
			st.nonce = nonce
		}
		return ctx
	}
	return withOpencodeRequest(ctx, nonce, nil, nil)
}

func withOpencodeSessions(ctx context.Context, sessions *opencode.SessionMap) context.Context {
	if sessions == nil {
		return ctx
	}
	if st := opencodeStateFrom(ctx); st != nil {
		if st.sessions == nil {
			st.sessions = sessions
		}
		return ctx
	}
	st := &opencodeReqState{sessions: sessions}
	return context.WithValue(ctx, opencodeStateKey, st)
}

func prepareOpencodeSession(ctx context.Context, provider *domain.Provider, body []byte) {
	st := opencodeStateFrom(ctx)
	var id opencode.Identity
	nonce := ""
	if st != nil {
		id = st.identity
		nonce = st.nonce
	}
	var providerID int64
	var baseURL, apiKey string
	if provider != nil {
		providerID = provider.ID
		baseURL = provider.BaseURL
		apiKey = provider.APIKey
	}
	transcript := opencode.AccumulateAssistantText(body)
	sid := opencode.ResolveSession(id, nonce, providerID, baseURL, apiKey, transcript)
	if st != nil && st.sessions != nil && opencode.NeedsFreeTier(baseURL, reqModelFromBody(body)) {
		if opencode.IsNativeSessionID(id.Session) {
			sid = id.Session
		} else if id.Session != "" {
			sid = st.sessions.ClientSession(id.Session)
		} else {
			sid = st.sessions.FallbackSession(nonce, providerID, baseURL, apiKey, transcript)
		}
	}
	if st != nil {
		st.session = sid
	}
	if t := traceInfoFrom(ctx); t != nil {
		t.OpencodeSessionID = sid
	}
}

func prepareOpencodeFreeTier(ctx context.Context, backend codec, provider *domain.Provider, body []byte) ([]byte, error) {
	st := opencodeStateFrom(ctx)
	if st != nil {
		st.tools = opencode.ToolMap{}
	}
	if provider == nil || !opencode.NeedsFreeTier(provider.BaseURL, reqModelFromBody(body)) {
		return body, nil
	}
	out, mapping, err := opencode.PrepareFreeTierBody(body, backend.id)
	if err != nil {
		return nil, err
	}
	if st != nil {
		st.tools = mapping
	}
	// The free gate also requires the CLI environment section in the system
	// prompt. Insertion is fail-open and idempotent; an existing caller block
	// (real CLI passthrough) is never duplicated.
	return opencode.AppendEnvironment(out, backend.id), nil
}

func opencodeToolMapFrom(ctx context.Context) opencode.ToolMap {
	st := opencodeStateFrom(ctx)
	if st == nil {
		return opencode.ToolMap{}
	}
	return st.tools
}

// applyClaudeCodeHeaders sets the pinned CLI fingerprint and the session id
// that must match metadata.user_id.session_id. It runs after the client-header
// copy so a forwarded identity does not replace the profile. Authorization is
// left to the auth-scheme switch. Beta flags are feature-gated from the body.
func applyClaudeCodeHeaders(req *http.Request, ctx context.Context, provider *domain.Provider, body []byte) {
	profile := claudeProfileFrom(ctx)
	for k, v := range claudecode.StainlessRuntimeHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("anthropic-version", claudecode.AnthropicVersion)
	req.Header.Set("User-Agent", profile.UserAgent())
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	beta := claudeBetaRequest(ctx, provider, body)
	beta.CallerBetas = mergeCallerBetas(beta.CallerBetas, requestCallerBetas(req.Header))
	req.Header.Set("anthropic-beta", claudecode.BetaHeaders(beta))
	if sid := claudeWireSession(ctx); sid != "" {
		req.Header.Set(claudecode.SessionIDHeader, sid)
	}
}

func claudeBetaHeader(ctx context.Context, provider *domain.Provider, body []byte) string {
	return claudecode.BetaHeaders(claudeBetaRequest(ctx, provider, body))
}

// mergeCallerBetas combines the validated context capture with values forwarded
// on the wire. Dedupe happens before the feature gate, so a duplicate cannot
// survive through a second source.
func mergeCallerBetas(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, flag := range list {
			if flag == "" || seen[flag] {
				continue
			}
			seen[flag] = true
			out = append(out, flag)
		}
	}
	return out
}

func requestCallerBetas(h http.Header) []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, value := range h.Values("anthropic-beta") {
		out = append(out, claudecode.ParseCallerBetas(value)...)
	}
	return out
}

func claudeBetaRequest(ctx context.Context, provider *domain.Provider, body []byte) claudecode.BetaRequest {
	var probe struct {
		Model string `json:"model"`
		Tools []struct {
			Type         string `json:"type"`
			DeferLoading *bool  `json:"defer_loading"`
		} `json:"tools"`
		ContextManagement json.RawMessage `json:"context_management"`
		OutputConfig      *struct {
			Effort     string          `json:"effort"`
			Format     json.RawMessage `json:"format"`
			TaskBudget json.RawMessage `json:"task_budget"`
		} `json:"output_config"`
		Thinking *struct {
			Type    string `json:"type"`
			Display string `json:"display"`
		} `json:"thinking"`
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(body, &probe)
	req := claudecode.BetaRequest{
		Model:       probe.Model,
		OAuth:       claudecode.IsOAuthToken(providerToken(provider)),
		CallerBetas: claudeCallerBetas(ctx),
	}
	if len(probe.ContextManagement) > 0 && probe.ContextManagement[0] == '{' {
		req.ContextManagement = true
	}
	if probe.OutputConfig != nil {
		req.Effort = probe.OutputConfig.Effort != ""
		req.StructuredOutput = len(probe.OutputConfig.Format) > 0 && probe.OutputConfig.Format[0] == '{'
		req.TaskBudget = validTaskBudgetFeature(probe.OutputConfig.TaskBudget)
	}
	if probe.Thinking != nil {
		req.ThinkingActive = probe.Thinking.Type == "adaptive" || probe.Thinking.Type == "enabled"
		req.ThinkingSummarized = probe.Thinking.Display == "summarized"
		if probe.Thinking.Type == "adaptive" {
			req.Effort = true
		}
	}
	for _, t := range probe.Tools {
		if t.Type != "" || (t.DeferLoading != nil && *t.DeferLoading) {
			req.AdvancedTools = true
		}
	}
	req.CacheScope, req.ExtendedTTL, req.CacheEvict = claudecode.CacheFeatures(body)
	req.OneMillion = bodyRequestsOneMillion(probe.Model, body)
	if st := claudeStateFrom(ctx); st != nil && st.contextOneMillion {
		req.OneMillion = true
		req.ContextOneMillion = true
	}
	return req
}

// bodyRequestsOneMillion reports a structured long-context request. A model
// id suffix [1m] is the CLI opt-in. A top-level betas array or metadata.betas
// entry equal to the long-context flag is also structured. A quoted flag
// inside schema text is not.
func bodyRequestsOneMillion(model string, body []byte) bool {
	if strings.Contains(strings.ToLower(model), "[1m]") {
		return true
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	if betaListHas(doc["betas"], claudecode.BetaLongContext) {
		return true
	}
	var meta struct {
		Betas []string `json:"betas"`
	}
	if json.Unmarshal(doc["metadata"], &meta) == nil {
		for _, flag := range meta.Betas {
			if flag == claudecode.BetaLongContext {
				return true
			}
		}
	}
	return false
}

// validTaskBudgetFeature accepts only the verified output_config.task_budget
// object shape. A schema key or a bare string is not a task-budget request.
func validTaskBudgetFeature(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var budget struct {
		Type      string `json:"type"`
		Total     *int   `json:"total"`
		Remaining *int   `json:"remaining"`
	}
	if json.Unmarshal(raw, &budget) != nil {
		return false
	}
	if budget.Type != "tokens" || budget.Total == nil || *budget.Total < 0 {
		return false
	}
	return budget.Remaining == nil || *budget.Remaining >= 0
}

func betaListHas(raw json.RawMessage, want string) bool {
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	var flags []string
	if json.Unmarshal(raw, &flags) != nil {
		return false
	}
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
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

// upstreamClient selects the HTTP client for one upstream send. Official
// OpenCode free-tier inference requests from opencode-protocol providers use
// the impersonated-TLS client (see internal/proxy/opencode/transport.go);
// every other request, including other protocols stored on an official URL,
// keeps the configured client unchanged. A replaced client or streamClient
// (tests inject capture and redirect transports) is detected by pointer
// inequality against the New-built clients and always wins, so injection is
// never bypassed.
func (p *Proxy) upstreamClient(provider *domain.Provider, url string, body []byte, stream bool) *http.Client {
	client := p.streamClient
	base := p.baseStreamClient
	if !stream {
		client = p.client
		base = p.baseUnaryClient
	}
	if client == nil {
		return nil
	}
	if base != nil && client != base {
		return client
	}
	if p.freeTierClient != nil &&
		provider != nil &&
		provider.Protocol == domain.ProtocolOpencode &&
		opencode.IsOfficialFreeTierURL(url) &&
		opencode.NeedsFreeTier(effectiveProviderBaseURL(provider), reqModelFromBody(body)) {
		return p.freeTierClient
	}
	return client
}

// forward sends the prepared body to the provider's upstream endpoint for the
// given backend protocol, setting the protocol-appropriate auth headers.
// clientHeaders, when non-nil (passthrough), are forwarded under the denylist.
//
// For oauth providers the access token is resolved (and proactively refreshed
// when near expiry) before the first send; on a 401/403 the token is forcibly
// refreshed and the request retried once.
func (p *Proxy) forward(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header) (int, []byte, error) {
	url := strings.TrimRight(effectiveProviderBaseURL(provider), "/") + path
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	send := func() (int, []byte, error) {
		started := time.Now()
		wire, err := claudeWireBody(ctx, provider, path, body)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(wire))
		if err != nil {
			return 0, nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders, ctx, wire)
		// Snapshot headers after auth/identity so HAR sees the wire credentials.
		var harHdr http.Header
		if harRecorder(ctx) != nil {
			harHdr = req.Header.Clone()
		}
		resp, err := p.upstreamClient(provider, url, wire, false).Do(req)
		if err != nil {
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, wire, 0, nil, nil, "", 0, err.Error())
			}
			return 0, nil, err
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxUnaryUpstreamResponseBytes+1))
		if err != nil {
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, wire, resp.StatusCode, resp.Header, respBody, "", len(respBody), err.Error())
			}
			return resp.StatusCode, nil, err
		}
		if int64(len(respBody)) > maxUnaryUpstreamResponseBytes {
			err := fmt.Errorf("upstream response too large (exceeds %d bytes)", maxUnaryUpstreamResponseBytes)
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, wire, resp.StatusCode, resp.Header, respBody, "", len(respBody), err.Error())
			}
			return resp.StatusCode, nil, err
		}
		if harRecorder(ctx) != nil {
			p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, wire, resp.StatusCode, resp.Header, respBody, "", len(respBody), "")
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
// upstreamURL resolves the request URL for a backend path. An absolute path
// (http/https) is used verbatim — backends whose chat host differs from the
// provider's base URL (Cursor AgentService) supply one — unless the provider
// carries a non-Cursor base URL, which then overrides the absolute host
// (self-hosted mirrors and tests).
func upstreamURL(provider *domain.Provider, path string) string {
	if provider != nil && provider.Protocol == domain.ProtocolKiro && kiro.UseRuntime(kiro.IdentityFromProvider(provider)) {
		return kiro.ChatURL(provider.BaseURL, kiro.IdentityFromProvider(provider))
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if b := strings.TrimSpace(provider.BaseURL); b != "" && !strings.Contains(b, "cursor.sh") {
			if u, err := url.Parse(path); err == nil {
				if bu, err := url.Parse(strings.TrimRight(b, "/")); err == nil && bu.Host != "" {
					u.Scheme = bu.Scheme
					u.Host = bu.Host
					return u.String()
				}
			}
		}
		return path
	}
	return strings.TrimRight(effectiveProviderBaseURL(provider), "/") + path
}

// effectiveProviderBaseURL returns the provider's stored base URL with
// backend-specific endpoint migrations applied. Only OpenCode rows move; every
// other protocol keeps its stored URL byte-for-byte, and the provider record
// itself is never mutated.
func effectiveProviderBaseURL(provider *domain.Provider) string {
	if provider == nil {
		return ""
	}
	if provider.Protocol == domain.ProtocolOpencode {
		return opencode.EffectiveBaseURL(provider.BaseURL)
	}
	return provider.BaseURL
}

func (p *Proxy) forwardStream(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header, streamAccept string) (*http.Response, error) {
	url := upstreamURL(provider, path)
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	accept := streamAccept
	if accept == "" {
		accept = "text/event-stream"
	}
	send := func(ctx context.Context) (*http.Response, error) {
		started := time.Now()
		wire, err := claudeWireBody(ctx, provider, path, body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(wire))
		if err != nil {
			return nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders, ctx, wire)
		req.Header.Set("Accept", accept)
		var harHdr http.Header
		if harRecorder(ctx) != nil {
			harHdr = req.Header.Clone()
		}
		resp, err := p.upstreamClient(provider, url, wire, true).Do(req)
		if err != nil {
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, wire, 0, nil, nil, "", 0, err.Error())
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
				reqBody: wire,
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
	resp, err := send(ctx)
	if err != nil {
		return resp, err
	}
	if isAuthFailure(resp.StatusCode) && provider.Method() == domain.AuthOAuth {
		if rerr := p.resolveToken(ctx, provider, true); rerr == nil {
			resp.Body.Close()
			return send(ctx)
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
	// Qoder answers model-queue refusals as a 200 SSE whose first frame is a
	// 10605 envelope. Recovery replays the same prepared body with fresh
	// signing until the model drains or the recovery budget expires.
	if provider.Protocol == domain.ProtocolQoder && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return p.recoverQoderQueue(ctx, provider, send, resp)
	}
	return resp, nil
}

// forwardStreamDuplex opens a streaming request whose body stays writable
// after the initial bytes are sent (bidi Connect over HTTP/2; the pipe forces
// chunked/streamed body framing). It returns the live response plus a frame
// writer bound to the winning attempt's request body and a closeWrite that
// ends the body — the caller must invoke closeWrite once decoding finishes or
// the attempt is abandoned. The reactive OAuth retry mirrors forwardStream,
// with a fresh pipe per attempt (a consumed pipe cannot be replayed).
func (p *Proxy) forwardStreamDuplex(ctx context.Context, provider *domain.Provider, path string, body []byte, clientHeaders http.Header, streamAccept string) (*http.Response, func([]byte) error, func(), error) {
	url := upstreamURL(provider, path)
	if t := traceInfoFrom(ctx); t != nil {
		t.UpstreamURL = url
	}
	accept := streamAccept
	if accept == "" {
		accept = "text/event-stream"
	}

	// pw and liveResp are touched from the caller goroutine (send/retry), the
	// decode loop's writeFrame, and the cancel watcher below; mu guards both.
	// Pipe writes run outside the lock so a stalled upstream cannot hold it.
	var mu sync.Mutex
	var pw *io.PipeWriter
	var liveResp io.ReadCloser
	closeWrite := func() {
		mu.Lock()
		defer mu.Unlock()
		if pw != nil {
			pw.Close()
			pw = nil
		}
	}
	writeFrame := func(frame []byte) error {
		mu.Lock()
		w := pw
		mu.Unlock()
		if w == nil {
			return fmt.Errorf("proxy: duplex request body already closed")
		}
		_, err := w.Write(frame)
		return err
	}
	// The HTTP/2 client does not abort an in-flight response read on context
	// cancellation while the request body is still open, and a bidi stream
	// keeps its body open by design — so a client disconnect would leave the
	// decode loop blocked in Read forever (and pin any HAR lease). Closing the
	// response body is the only reliable unblock; callers classify the
	// resulting read error as a client disconnect via ctx.Err().
	go func() {
		<-ctx.Done()
		closeWrite()
		mu.Lock()
		defer mu.Unlock()
		if liveResp != nil {
			liveResp.Close()
			liveResp = nil
		}
	}()

	send := func() (*http.Response, error) {
		closeWrite() // release the previous attempt's pipe, if any
		pr, npw := io.Pipe()
		mu.Lock()
		pw = npw
		mu.Unlock()
		wire, werr := claudeWireBody(ctx, provider, path, body)
		if werr != nil {
			closeWrite()
			return nil, werr
		}
		go func() {
			// Initial request bytes; the pipe stays open for mid-stream replies.
			if _, err := npw.Write(wire); err != nil {
				npw.CloseWithError(err)
			}
		}()
		started := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
		if err != nil {
			closeWrite()
			return nil, err
		}
		applyUpstreamHeaders(req, provider, clientHeaders, ctx, wire)
		req.Header.Set("Accept", accept)
		var harHdr http.Header
		if harRecorder(ctx) != nil {
			harHdr = req.Header.Clone()
		}
		resp, err := p.streamClient.Do(req)
		if err != nil {
			closeWrite()
			if harRecorder(ctx) != nil {
				p.recordUpstreamHAR(ctx, started, time.Since(started), req.Method, url, harHdr, wire, 0, nil, nil, "", 0, err.Error())
			}
			return nil, err
		}
		if harRecorder(ctx) != nil {
			// HAR captures the initial request bytes only; mid-stream duplex
			// replies are not recorded.
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
		mu.Lock()
		liveResp = resp.Body
		mu.Unlock()
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
		return nil, writeFrame, closeWrite, err
	}
	if isAuthFailure(resp.StatusCode) && provider.Method() == domain.AuthOAuth {
		if rerr := p.resolveToken(ctx, provider, true); rerr == nil {
			resp.Body.Close()
			resp, err = send()
			if err != nil {
				return nil, writeFrame, closeWrite, err
			}
			return resp, writeFrame, closeWrite, nil
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
	return resp, writeFrame, closeWrite, nil
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
	if provider == nil || p == nil || p.oauth == nil {
		return nil
	}
	// Method gates the credential. Scheme and protocol do not. An API-key
	// provider may keep config-only OAuthCreds; those creds must not replace
	// APIKey, including when a stale AccessToken is present.
	if provider.Method() != domain.AuthOAuth {
		return nil
	}
	creds, err := p.oauth.ResolveCreds(ctx, provider, force)
	if creds == nil {
		return err
	}
	// Install the paired snapshot before the token. A later header or body
	// read must see the account and device that produced this access token.
	cp := *creds
	provider.OAuthCreds = &cp
	if cp.AccessToken != "" {
		provider.APIKey = cp.AccessToken
	}
	return err
}

// isAuthFailure reports whether an upstream status indicates a rejected
// credential, the trigger for a reactive OAuth token refresh.
func isAuthFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}
