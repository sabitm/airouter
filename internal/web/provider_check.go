package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/oauth"
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

// traceMaxBody caps the outbound /models body logged at trace level so a long
// model list cannot flood the terminal.
const traceMaxBody = 16 << 10

// checkUpstream performs a GET {base_url}/models with the protocol's auth
// headers and classifies the outcome. The /models response shape is identical
// across OpenAI and Anthropic, so protocol verification is a soft signal: a
// mismatch surfaces only via a 404 or an unexpected body, not definitively.
//
// When trace is set the request and response are logged; auth headers are never
// logged, so the API key stays out of the log.
func checkUpstream(ctx context.Context, p *domain.Provider, trace bool) (bool, string) {
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
		log.Printf("[trace] <<< %d\n%s", resp.StatusCode, traceBody(body))
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

// traceBody renders an outbound response body for the log, truncating to
// traceMaxBody with a marker when the full body is longer.
func traceBody(body []byte) string {
	if len(body) == 0 {
		return "(empty)"
	}
	if len(body) > traceMaxBody {
		return fmt.Sprintf("%s... (truncated, %d bytes total)", body[:traceMaxBody], len(body))
	}
	return string(body)
}
