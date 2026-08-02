package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/qoder"
)

// checkProvider validates a base URL + credential + protocol against the live
// upstream before the provider is saved. For apikey providers the api_key field
// may be blank on an edit, in which case the stored key for the given id is
// reused. For oauth providers there is no api_key: the connection's stored
// access token (resolved/refreshed) is used, so a Check confirms the OAuth
// credential is currently valid.
func (h *Handler) checkProvider(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, CheckResult(false, "invalid form"))
		return
	}
	proto := domain.Protocol(r.FormValue("protocol"))
	if !proto.Valid() {
		render(w, r, CheckResult(false, "select a protocol"))
		return
	}
	auth := domain.AuthScheme(r.FormValue("auth_scheme"))
	if auth != "" && !auth.Valid() {
		render(w, r, CheckResult(false, "select an auth scheme"))
		return
	}
	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	if baseURL == "" {
		render(w, r, CheckResult(false, "enter a base URL"))
		return
	}

	method := domain.AuthMethod(r.FormValue("auth_method"))
	if method == domain.AuthOAuth {
		h.checkOAuthProvider(w, r, baseURL, proto)
		return
	}

	apiKey := r.FormValue("api_key")
	if apiKey == "" {
		// Edit form left the key blank to keep the current one; recover it.
		if id, err := strconv.ParseInt(r.FormValue("id"), 10, 64); err == nil {
			if p, err := h.store.GetProvider(r.Context(), id); err == nil {
				apiKey = p.APIKey
			}
		}
	}
	if apiKey == "" {
		render(w, r, CheckResult(false, "enter an API key"))
		return
	}

	ok, msg := checkUpstream(r.Context(), &domain.Provider{BaseURL: baseURL, APIKey: apiKey, Protocol: proto, AuthScheme: auth}, h.trace)
	render(w, r, CheckResult(ok, msg))
}

// checkOAuthProvider resolves an oauth provider's access token and probes the
// upstream with it. Two sources are accepted: a saved provider loaded by id
// (edit form, already connected) or an in-flight connect session by its state
// token (create form, connected but not yet saved). Without either, it reports
// that connect is needed.
func (h *Handler) checkOAuthProvider(w http.ResponseWriter, r *http.Request, baseURL string, proto domain.Protocol) {
	creds, fromStore := h.oauthCheckCreds(r)
	if creds == nil {
		render(w, r, CheckResult(false, "not connected yet - run Connect first"))
		return
	}
	probe := &domain.Provider{
		BaseURL: baseURL, Protocol: proto,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: creds,
	}
	// A saved provider can be refreshed and the rotated token persisted (it has a
	// store id); a session's token is probed as-is - refreshing it would write
	// nowhere and the just-connected token is fresh anyway.
	if fromStore {
		if id, err := strconv.ParseInt(r.FormValue("id"), 10, 64); err == nil {
			probe.ID = id
		}
		tok, err := h.oauth.Resolve(r.Context(), probe, false)
		if err != nil {
			if oauth.IsInvalidGrant(err) {
				render(w, r, CheckResult(false, "token expired - reconnect required"))
				return
			}
			render(w, r, CheckResult(false, "token refresh failed: "+err.Error()))
			return
		}
		probe.APIKey = tok
	} else {
		probe.APIKey = creds.AccessToken
	}
	ok, msg := checkUpstream(r.Context(), probe, h.trace)
	render(w, r, CheckResult(ok, msg))
}

// oauthCheckCreds finds the credentials to probe for an oauth Check, in the same
// precedence Save uses: a just-completed connect session, then tokens pasted into
// the form, then the saved provider's stored creds (by id). fromStore is true
// only for stored creds - the source whose refresh can be persisted; session and
// pasted tokens are probed as-is.
func (h *Handler) oauthCheckCreds(r *http.Request) (creds *domain.OAuthCreds, fromStore bool) {
	if c, ok := h.connectedCreds(r.FormValue("oauth_session")); ok {
		return c, false
	}
	if c, err := credsFromConnectForm(r); err == nil && applyManualTokens(c, r) {
		return c, false
	}
	if id, err := strconv.ParseInt(r.FormValue("id"), 10, 64); err == nil {
		if p, err := h.store.GetProvider(r.Context(), id); err == nil &&
			p.OAuthCreds != nil && p.OAuthCreds.AccessToken != "" {
			return p.OAuthCreds, true
		}
	}
	return nil, false
}

// traceMaxBody caps the outbound /models body logged to stderr at trace level so
// a long model list cannot flood the terminal. Full request/response forensics
// live in HAR capture (-har-file).
const traceMaxBody = 16 << 10

// checkUpstream performs a GET {base_url}/models with the protocol's auth
// headers and classifies the outcome. The /models response shape is identical
// across OpenAI and Anthropic, so protocol verification is a soft signal: a
// mismatch surfaces only via a 404 or an unexpected body, not definitively.
//
// When trace is set the request and response are logged; auth headers are never
// logged, so the API key stays out of the log.
func checkUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	if p.Protocol == domain.ProtocolOpenAICodex {
		return checkCodexUpstream(ctx, p, trace)
	}
	if p.Protocol == domain.ProtocolKiro {
		return checkKiroUpstream(ctx, p, trace)
	}
	if p.Protocol == domain.ProtocolQoder {
		return checkQoderUpstream(ctx, p, trace)
	}
	if p.Protocol == domain.ProtocolAntigravity {
		return checkAntigravityUpstream(ctx, p, trace)
	}
	if p.Protocol == domain.ProtocolCursor {
		return checkCursorUpstream(ctx, p, trace)
	}
	if p.Protocol == domain.ProtocolClaudeCode {
		return checkClaudeCodeUpstream(ctx, p, trace)
	}
	if p.OAuthCreds != nil && p.OAuthCreds.ClineAuth {
		return checkClineUpstream(ctx, p, trace)
	}
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "invalid base URL"
	}
	// Match the auth scheme the proxy would actually use (see applyUpstreamHeaders),
	// so a passing Check implies the credential will be accepted on real traffic.
	switch p.Auth() {
	case domain.AuthXAPIKey:
		req.Header.Set("x-api-key", p.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	if p.Protocol == domain.ProtocolAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	applyClineProbeHeaders(req, p)

	if trace {
		log.Printf("[trace] >>> GET %s", url)
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< GET %s: %v", url, err)
		}
		return false, "could not reach URL: " + err.Error()
	}
	defer resp.Body.Close()

	// Read the body before classifying so the trace covers every status, not
	// just the success path.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body, traceMaxBody))
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("API key rejected (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return false, "not found (HTTP 404) - check base URL and protocol"
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Data == nil {
		return false, "reachable, but response shape unexpected - protocol may not match"
	}
	return true, fmt.Sprintf("OK - reachable, key accepted (%d models)", len(parsed.Data))
}

// checkClaudeCodeUpstream validates a Claude Code OAuth token against the
// Anthropic /models endpoint using the Claude Code CLI identity fingerprint, so
// a passing Check implies the credential and client profile will be accepted on
// real traffic. OAuth tokens are resolved/refreshed by the caller before this
// runs, so p.APIKey holds the live access token.
func checkClaudeCodeUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "invalid base URL"
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	for k, v := range claudecode.IdentityHeaders() {
		req.Header.Set(k, v)
	}

	if trace {
		log.Printf("[trace] >>> GET %s", url)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< GET %s: %v", url, err)
		}
		return false, "could not reach URL: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body, traceMaxBody))
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("token rejected (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return false, "not found (HTTP 404) - check base URL"
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Data == nil {
		return false, "reachable, but response shape unexpected"
	}
	return true, fmt.Sprintf("OK - reachable, token accepted (%d models)", len(parsed.Data))
}

// checkQoderUpstream validates a Qoder device token against openapi userinfo.
// Uses plain Bearer auth (not COSY); refresh is not attempted.
func checkQoderUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	url := qoder.UserInfoURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "invalid request"
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	if trace {
		log.Printf("[trace] >>> GET %s", url)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< GET %s: %v", url, err)
		}
		return false, "could not reach URL: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body, traceMaxBody))
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return false, "token invalid or revoked (HTTP 401)"
	case resp.StatusCode == http.StatusForbidden:
		return false, "access denied (HTTP 403)"
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	return true, "OK - reachable, token accepted"
}

// checkClineUpstream validates a Cline/ClinePass OAuth token against Cline's
// /users/me endpoint. Plain Cline does not expose /models, so probing it (the
// generic path) returns a misleading 404 even for a valid token. /users/me is
// what 9router uses to verify the access token.
func checkClineUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	url := strings.TrimRight(p.BaseURL, "/") + "/users/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "invalid base URL"
	}
	applyClineProbeHeaders(req, p)
	req.Header.Set("Accept", "application/json")

	if trace {
		log.Printf("[trace] >>> GET %s", url)
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< GET %s: %v", url, err)
		}
		return false, "could not reach URL: " + err.Error()
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body, traceMaxBody))
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return false, "token invalid or revoked (HTTP 401)"
	case resp.StatusCode == http.StatusForbidden:
		return false, "access denied (HTTP 403)"
	case resp.StatusCode == http.StatusNotFound:
		return false, "not found (HTTP 404) - check base URL"
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	return true, "OK - reachable, token accepted"
}

// checkCodexUpstream validates the ChatGPT Codex model-discovery endpoint. It is
// account-aware and avoids hardcoding a probe model that may not be available to
// this ChatGPT account.
func checkCodexUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	models, status, body, err := fetchCodexModels(ctx, p, trace)
	if err != nil {
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return false, fmt.Sprintf("API key rejected (HTTP %d)", status)
		case status == http.StatusNotFound:
			return false, "not found (HTTP 404) - check base URL and protocol"
		case status >= 400:
			return false, fmt.Sprintf("upstream returned HTTP %d: %s", status, upstreamErrorText(body))
		default:
			return false, "could not reach URL: " + err.Error()
		}
	}
	return true, fmt.Sprintf("OK - Codex models reachable, token accepted (%d models)", len(models))
}

func newCheckSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func upstreamErrorText(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(body)
}

// applyClineProbeHeaders overlays Cline identity headers (and the workos:
// bearer prefix) when the provider is a Cline/ClinePass OAuth connection, so
// dashboard Check and /models probes match what the proxy sends upstream.
func applyClineProbeHeaders(req *http.Request, p *domain.Provider) {
	if p == nil || p.OAuthCreds == nil || !p.OAuthCreds.ClineAuth {
		return
	}
	for k, v := range oauth.ClineIdentityHeaders("", p.APIKey) {
		req.Header.Set(k, v)
	}
}

// traceBody renders an outbound response body for the log, appending a marker
// when the output was capped. limit <= 0 logs the whole body.
func traceBody(body []byte, limit int) string {
	if len(body) == 0 {
		return "(empty)"
	}
	if limit > 0 && len(body) > limit {
		return fmt.Sprintf("%s... (truncated, %d bytes total)", body[:limit], len(body))
	}
	return string(body)
}

// checkAntigravityUpstream validates token + project via loadCodeAssist.
func checkAntigravityUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	const url = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	meta := map[string]int{"ideType": 9, "platform": 3, "pluginType": 2}
	payload, _ := json.Marshal(map[string]any{"metadata": meta})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return false, "invalid request"
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "google-api-nodejs-client/9.15.1")
	req.Header.Set("X-Goog-Api-Client", "google-cloud-sdk vscode_cloudshelleditor/0.1")
	mb, _ := json.Marshal(meta)
	req.Header.Set("Client-Metadata", string(mb))

	if trace {
		log.Printf("[trace] >>> POST %s", url)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< POST %s: %v", url, err)
		}
		return false, "could not reach URL: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body, traceMaxBody))
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("token rejected (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	var data map[string]any
	_ = json.Unmarshal(body, &data)
	project := ""
	switch v := data["cloudaicompanionProject"].(type) {
	case string:
		project = strings.TrimSpace(v)
	case map[string]any:
		if id, ok := v["id"].(string); ok {
			project = strings.TrimSpace(id)
		}
	}
	if project == "" && p.OAuthCreds != nil {
		project = p.OAuthCreds.ProjectID
	}
	if project == "" {
		return false, "OK token, but no Cloud Code project - reconnect OAuth"
	}
	return true, fmt.Sprintf("OK - reachable, project %s", project)
}

// checkCursorUpstream validates a Cursor IDE token against the AgentService
// GetUsableModels endpoint. Cursor's ChatService is Connect-RPC protobuf and
// has no /models REST endpoint; GetUsableModels is the lighter liveness probe
// (an unframed application/proto unary call). Tokens are short-lived and not
// refreshable, so a 401/403 means re-paste, not refresh.
func checkCursorUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
	token := strings.TrimSpace(p.APIKey)
	if token == "" {
		return false, "no access token - paste one or reconnect"
	}
	machineID := ""
	if p.OAuthCreds != nil {
		machineID = strings.TrimSpace(p.OAuthCreds.MachineID)
	}
	url := cursor.AgentBaseURL + cursor.ModelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(""))
	if err != nil {
		return false, "invalid request"
	}
	h := cursor.BuildModelsHeaders(token, machineID, true)
	for k, vv := range h {
		for _, v := range vv {
			req.Header.Set(k, v)
		}
	}
	if trace {
		log.Printf("[trace] >>> POST %s", url)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		if trace {
			log.Printf("[trace] <<< POST %s: %v", url, err)
		}
		return false, "could not reach Cursor: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if trace {
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body, traceMaxBody))
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("token rejected (HTTP %d) - re-paste required", resp.StatusCode)
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	ids := cursor.ParseUsableModels(body)
	return true, fmt.Sprintf("OK - reachable, token accepted (%d models)", len(ids))
}
