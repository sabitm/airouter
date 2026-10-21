package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/thinking"
)

const (
	claudeSessionHeader = "x-claude-code-session-id"
	claudePromptHeader  = "x-claude-code-prompt-id"
	claudeParentHeader  = "x-claude-code-parent-session-id"
	claudeAgentHeader   = "x-claude-code-agent-id"
)

// claudeReqState is the original client identity captured before translation.
// The session seed is stable for the request, including retries. The wire id is
// scoped from that seed against the current provider snapshot each send, so a
// token refresh that changes the account can change the scope. Account is never
// taken from the client.
type claudeReqState struct {
	session string
	// anonymous is generated once for an incoming request that supplied no
	// session id. It is the unscoped seed retries scope against the current
	// provider snapshot, so an OAuth refresh that changes the account gets a
	// session that matches the new token.
	anonymous string
	// wireSession is the scoped id derived for the latest send after token
	// resolution. The header reads it so a body and header always agree.
	wireSession    string
	parent         string
	promptID       string
	agentID        string
	entrypoint     string
	sdkVersion     string
	clientApp      string
	workload       string
	nonInteractive bool
	appendSystem   bool
	// callerBetas is the validated anthropic-beta list captured before
	// translation. The header seam merges only flags the model still supports.
	callerBetas []string
	// contextOneMillion is the current attempt's target [1m] selector. It is
	// reset on every Claude Code attempt so failover cannot inherit it.
	contextOneMillion bool
}

type claudeStateKeyT struct{}

var claudeStateKey claudeStateKeyT

type claudeTenantKeyT struct{}

var claudeTenantKey claudeTenantKeyT

func withClaudeTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		tenant = openTenantScope
	}
	return context.WithValue(ctx, claudeTenantKey, tenant)
}

func claudeTenantFrom(ctx context.Context) string {
	t, _ := ctx.Value(claudeTenantKey).(string)
	if t == "" {
		return openTenantScope
	}
	return t
}

func withClaudeRequest(ctx context.Context, h http.Header, body []byte) context.Context {
	st := &claudeReqState{}
	st.capture(h, body)
	return context.WithValue(ctx, claudeStateKey, st)
}

func claudeStateFrom(ctx context.Context) *claudeReqState {
	st, _ := ctx.Value(claudeStateKey).(*claudeReqState)
	return st
}

// applyClaudeCodeTargetModel removes recognized context selectors from the
// Claude Code wire model and records the selector intent for this attempt. The
// pinned CLI strips the tag from the API model but still sends the long-context
// beta for a [1m] tag. Other protocols keep their model spelling unchanged.
func applyClaudeCodeTargetModel(ctx context.Context, provider *domain.Provider, model string) string {
	if provider == nil || provider.Protocol != domain.ProtocolClaudeCode {
		return model
	}
	if st := claudeStateFrom(ctx); st != nil {
		st.contextOneMillion = thinking.HasOneMillionContextSelector(model)
	}
	return thinking.StripContextSelectors(model)
}

func hasClaudeCodeTarget(targets []domain.ComboTarget) bool {
	for _, target := range targets {
		if target.Provider != nil && target.Provider.Protocol == domain.ProtocolClaudeCode {
			return true
		}
	}
	return false
}

func (st *claudeReqState) capture(h http.Header, body []byte) {
	if st == nil {
		return
	}
	if h != nil {
		if v := singleHeader(h, claudeSessionHeader); validClaudeUUID(v) {
			st.session = v
		}
		if v := singleHeader(h, claudeParentHeader); validClaudeUUID(v) {
			st.parent = v
		}
		if v := singleHeader(h, claudePromptHeader); validClaudeUUID(v) {
			st.promptID = v
		}
		if v := singleHeader(h, claudeAgentHeader); validClaudeUUID(v) {
			st.agentID = v
		}
		st.entrypoint = validatedEntrypoint(singleHeader(h, "x-claude-code-entrypoint"))
		st.sdkVersion = validatedSDKVersion(singleHeader(h, "x-claude-agent-sdk-version"))
		st.clientApp = validatedSuffix(singleHeader(h, "x-client-app"))
		st.workload = validatedSuffix(singleHeader(h, "x-claude-code-workload"))
		st.callerBetas = claudecode.ParseCallerBetas(singleHeader(h, "anthropic-beta"))
	}
	var meta struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		SessionID string `json:"session_id"`
	}
	if len(body) > 0 && json.Unmarshal(body, &meta) == nil {
		if st.session == "" && validClaudeUUID(meta.SessionID) {
			st.session = meta.SessionID
		}
		if parsed := parseUserID(meta.Metadata.UserID); parsed != nil {
			if st.session == "" && validClaudeUUID(parsed.SessionID) {
				st.session = parsed.SessionID
			}
			if st.parent == "" && validClaudeUUID(parsed.ParentSessionID) {
				st.parent = parsed.ParentSessionID
			}
		}
	}
	// prompt_id in the body is not the CLI prompt header. Do not read it.
	if h != nil {
		ua := strings.ToLower(singleHeader(h, "user-agent"))
		if st.sdkVersion == "" {
			st.sdkVersion = sdkVersionFromUA(ua)
		}
		if st.entrypoint == "" {
			st.entrypoint = entrypointFromUA(ua)
		}
		if strings.Contains(ua, "claude-agent-sdk") && st.entrypoint == "" {
			st.nonInteractive = true
		}
	}
	if st.session == "" {
		st.anonymous = newUUID()
	}
}

type parsedUserID struct {
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id"`
}

func parseUserID(raw string) *parsedUserID {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return nil
	}
	var id parsedUserID
	if json.Unmarshal([]byte(raw), &id) != nil {
		return nil
	}
	return &id
}

func validatedEntrypoint(s string) string {
	s = strings.TrimSpace(s)
	switch s {
	case "cli", "sdk-cli", "sdk-ts", "sdk-py", "mcp", "claude-vscode", "remote":
		return s
	default:
		return ""
	}
}

func validatedSDKVersion(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 32 {
		return ""
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			return ""
		}
	}
	return s
}

func validatedSuffix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return ""
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return ""
		}
	}
	return s
}

func sdkVersionFromUA(ua string) string {
	const mark = "agent-sdk/"
	i := strings.Index(ua, mark)
	if i < 0 {
		return ""
	}
	rest := ua[i+len(mark):]
	if j := strings.IndexAny(rest, ", )"); j >= 0 {
		rest = rest[:j]
	}
	return validatedSDKVersion(rest)
}

func entrypointFromUA(ua string) string {
	const mark = "(external, "
	i := strings.Index(ua, mark)
	if i < 0 {
		return ""
	}
	rest := ua[i+len(mark):]
	if j := strings.IndexAny(rest, ",)"); j >= 0 {
		rest = rest[:j]
	}
	return validatedEntrypoint(strings.TrimSpace(rest))
}

func singleHeader(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	vals := h.Values(key)
	if len(vals) != 1 {
		return ""
	}
	return strings.TrimSpace(vals[0])
}

func validClaudeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// claudeProviderScope identifies the conversation tenant. It uses the stable
// provider id, the canonical effective base URL, and the Claude account UUID.
// Display name, API key, and access token are not part of the scope: a rename
// or refresh must not split a conversation, and a different provider, base, or
// account must not join one. AccountID is not a substitute. It can belong to
// another protocol and must not borrow Claude identity. An API-key row can
// carry stale config-only OAuthCreds, so the account is read only for OAuth.
func claudeProviderScope(p *domain.Provider) string {
	if p == nil {
		return "none"
	}
	acct, _ := claudeIdentity(p)
	return strconv.FormatInt(p.ID, 10) + "\x00" + claudeCanonicalBase(p.BaseURL) + "\x00" + acct
}

func claudeCanonicalBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// A blank source base uses the same default endpoint as the preset.
		raw = claudecode.DefaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.ToLower(raw), "/")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return strings.ToLower(u.Scheme) + "://" + u.Host + u.Path
}

// resolveClaudeSession returns a session id for this request. A supplied UUID
// and the request-local anonymous seed are both scoped to the tenant and the
// current provider snapshot. The seed is stable across retries, while a refresh
// that changes the account changes the scope. Unidentified requests never join
// each other because each incoming request has its own anonymous seed.
func (p *Proxy) resolveClaudeSession(ctx context.Context, provider *domain.Provider, tenant string) string {
	return claudeSessionID(ctx, provider, tenant)
}

func claudeSessionID(ctx context.Context, provider *domain.Provider, tenant string) string {
	seed := ""
	if st := claudeStateFrom(ctx); st != nil {
		seed = st.session
		if seed == "" {
			seed = st.anonymous
		}
	}
	if seed == "" {
		// No captured request state: there is no retry identity to preserve.
		seed = newUUID()
	}
	return claudeScopedSession(tenant, provider, seed)
}

// claudeWireSession returns the session id derived by the latest wire-body
// preparation. A request without TraceInfo still gets a matching header.
func claudeWireSession(ctx context.Context) string {
	if st := claudeStateFrom(ctx); st != nil && st.wireSession != "" {
		return st.wireSession
	}
	if t := traceInfoFrom(ctx); t != nil {
		return t.ClaudeCodeSessionID
	}
	return ""
}

func claudeScopedSession(tenant string, p *domain.Provider, session string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + claudeProviderScope(p) + "\x00" + strings.ToLower(session)))
	return formatUUID(sum[:16])
}

func formatUUID(b []byte) string {
	if len(b) < 16 {
		return newUUID()
	}
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

func claudeProfileFrom(ctx context.Context) claudecode.Profile {
	p := claudecode.DefaultProfile()
	st := claudeStateFrom(ctx)
	if st == nil {
		return p
	}
	if st.entrypoint != "" {
		p.Entrypoint = st.entrypoint
	}
	switch st.entrypoint {
	case "sdk-cli", "sdk-ts", "sdk-py":
		p.SDK = st.sdkVersion != ""
		p.NonInteractive = true
		p.HasAppendSystemPrompt = st.appendSystem
	default:
		p.NonInteractive = st.nonInteractive
		p.HasAppendSystemPrompt = st.appendSystem
	}
	p.AgentSDKVersion = st.sdkVersion
	p.ClientApp = st.clientApp
	p.Workload = st.workload
	return p
}

func claudePromptID(ctx context.Context) string {
	if st := claudeStateFrom(ctx); st != nil {
		return st.promptID
	}
	return ""
}

func claudeParentID(ctx context.Context, provider *domain.Provider) string {
	st := claudeStateFrom(ctx)
	if st == nil || st.parent == "" {
		return ""
	}
	// A supplied parent UUID gets the same tenant, provider, and account scope
	// as the main session. The raw client UUID is not written upstream.
	return claudeScopedSession(claudeTenantFrom(ctx), provider, st.parent)
}

func claudeCallerBetas(ctx context.Context) []string {
	st := claudeStateFrom(ctx)
	if st == nil || len(st.callerBetas) == 0 {
		return nil
	}
	out := make([]string, len(st.callerBetas))
	copy(out, st.callerBetas)
	return out
}
