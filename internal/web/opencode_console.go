package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/opencode"
	"airouter/internal/store"
)

func opencodeAuthFromForm(r *http.Request) string {
	if opencodeTierFromForm(r, "") == "go" && strings.TrimSpace(r.FormValue("opencode_auth")) == "console" {
		return "console"
	}
	return "apikey"
}

func opencodeConsoleTarget(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.FormValue("id"))
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid provider id")
	}
	return id, nil
}

func (h *Handler) opencodeConsoleBegin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, OAuthConnectError("invalid form"))
		return
	}
	if opencodeAuthFromForm(r) != "console" {
		render(w, r, OAuthConnectError("OpenCode Console sign-in is available only for the Go tier"))
		return
	}
	targetID, err := opencodeConsoleTarget(r)
	if err != nil {
		render(w, r, OAuthConnectError(err.Error()))
		return
	}
	conn, err := oauth.NewOpencodeConsoleConnect()
	if err != nil {
		render(w, r, OAuthConnectError("could not start OpenCode Console sign-in"))
		return
	}
	if err := conn.Start(r.Context()); err != nil {
		conn.Close()
		render(w, r, OAuthConnectError(err.Error()))
		return
	}
	h.sessions.putBound(conn.State(), conn, opencodeConsoleFlow, targetID, timeNow())
	render(w, r, OpencodeConsolePending(conn.State(), conn.VerificationURI(), conn.UserCode()))
}

func (h *Handler) opencodeConsoleChoose(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, OAuthConnectError("invalid form"))
		return
	}
	state := r.FormValue("state")
	sess, ok := h.sessions.get(state)
	if !ok || !sess.bound || sess.flow != opencodeConsoleFlow {
		render(w, r, OAuthStatusLine(state, false, "error", "", "connect session expired - start over"))
		return
	}
	conn, ok := sess.conn.(*oauth.OpencodeConsoleConnect)
	if !ok {
		render(w, r, OAuthStatusLine(state, false, "error", "", "connect session expired - start over"))
		return
	}
	if err := conn.ChooseWorkspace(r.FormValue("opencode_org_choice")); err != nil {
		phase, workspace, errMsg, orgs := conn.Status()
		if phase == "choice" {
			render(w, r, OpencodeConsoleChoice(state, err.Error(), workspaceViews(orgs)))
			return
		}
		_ = workspace
		render(w, r, OAuthStatusLine(state, false, "error", "", errMsg))
		return
	}
	_, workspace, _, _ := conn.Status()
	render(w, r, OpencodeConsoleConnected(state, workspace))
}

func (h *Handler) renderOpencodeConsoleStatus(w http.ResponseWriter, r *http.Request, conn *oauth.OpencodeConsoleConnect) {
	phase, workspace, errMsg, orgs := conn.Status()
	switch phase {
	case "choice":
		render(w, r, OpencodeConsoleChoice(conn.State(), "", workspaceViews(orgs)))
	case "connected":
		render(w, r, OpencodeConsoleConnected(conn.State(), workspace))
	case "error":
		render(w, r, OAuthStatusLine(conn.State(), false, "error", "", errMsg))
	default:
		render(w, r, OAuthStatusLine(conn.State(), true, "pending", "", ""))
	}
}

func workspaceViews(orgs []oauth.OpencodeWorkspace) []opencodeWorkspaceView {
	out := make([]opencodeWorkspaceView, 0, len(orgs))
	for _, org := range orgs {
		name := org.Name
		if name == "" {
			name = org.ID
		}
		out = append(out, opencodeWorkspaceView{ID: org.ID, Name: name})
	}
	return out
}

func (h *Handler) createOpencodeProvider(w http.ResponseWriter, r *http.Request) {
	if opencodeAuthFromForm(r) == "console" {
		h.createOpencodeConsoleProvider(w, r)
		return
	}
	auth := domain.AuthScheme(r.FormValue("auth_scheme"))
	if auth != "" && !auth.Valid() {
		htmxBadRequest(w, r, "provider-flash", "invalid auth scheme")
		return
	}
	apiKey := strings.TrimSpace(r.FormValue("api_key"))
	baseURL := providerBaseURLOrDefault(domain.ProtocolOpencode, r.FormValue("base_url"))
	var err error
	baseURL, apiKey, err = resolveOpencodeCredential(opencodeTierFromForm(r, baseURL), apiKey, nil)
	if err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	dialect, ok := parseReasoningDialectForm(r.FormValue("reasoning_dialect"), domain.ProtocolOpencode)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	tags, err := domain.ParseTagList(r.FormValue("tags"))
	if err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	p := &domain.Provider{
		Name:             r.FormValue("name"),
		BaseURL:          baseURL,
		APIKey:           apiKey,
		Protocol:         domain.ProtocolOpencode,
		AuthMethod:       domain.AuthAPIKey,
		AuthScheme:       domain.AuthBearer,
		ReasoningDialect: dialect,
		Tags:             tags,
	}
	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.renderProviderList(w, r)
}

func (h *Handler) createOpencodeConsoleProvider(w http.ResponseWriter, r *http.Request) {
	targetID, err := opencodeConsoleTarget(r)
	if err != nil || targetID != 0 {
		htmxBadRequest(w, r, "provider-flash", "OpenCode Console sign-in does not match this form")
		return
	}
	state := r.FormValue("oauth_session")
	creds, ok := h.sessions.claimOpencodeConsole(state, 0)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "sign in with OpenCode Console before saving")
		return
	}
	if err := h.writeNewOpencodeConsole(r, creds); err != nil {
		h.sessions.releaseClaim(state)
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.sessions.drop(state)
	h.renderProviderList(w, r)
}

func (h *Handler) writeNewOpencodeConsole(r *http.Request, creds *domain.OAuthCreds) error {
	dialect, ok := parseReasoningDialectForm(r.FormValue("reasoning_dialect"), domain.ProtocolOpencode)
	if !ok {
		return errors.New("invalid reasoning dialect")
	}
	tags, err := domain.ParseTagList(r.FormValue("tags"))
	if err != nil {
		return err
	}
	p := &domain.Provider{
		Name:             r.FormValue("name"),
		BaseURL:          opencode.GoBaseURL,
		Protocol:         domain.ProtocolOpencode,
		AuthMethod:       domain.AuthOAuth,
		AuthScheme:       domain.AuthBearer,
		OAuthCreds:       creds,
		ReasoningDialect: dialect,
		Tags:             tags,
	}
	if err := opencode.ValidateConsoleImport(p); err != nil {
		return err
	}
	return h.store.CreateProvider(r.Context(), p)
}

func (h *Handler) saveOpencodeProvider(w http.ResponseWriter, r *http.Request, cur *domain.Provider, basisBlob string) {
	fromMethod := cur.AuthMethod
	fromProto := cur.Protocol
	if opencodeAuthFromForm(r) == "console" {
		h.saveOpencodeConsole(w, r, cur, fromMethod, fromProto, basisBlob)
		return
	}
	h.saveOpencodeAPIKey(w, r, cur, fromMethod, fromProto, basisBlob)
}

func (h *Handler) saveOpencodeConsole(w http.ResponseWriter, r *http.Request, cur *domain.Provider, fromMethod domain.AuthMethod, fromProto domain.Protocol, basisBlob string) {
	state := r.FormValue("oauth_session")
	reconnected := false
	if state != "" {
		creds, ok := h.sessions.claimOpencodeConsole(state, cur.ID)
		if !ok {
			htmxBadRequest(w, r, "provider-flash", "OpenCode Console sign-in does not match this provider")
			return
		}
		cur.OAuthCreds = creds
		reconnected = true
	}
	dialect, ok := parseProviderReasoningDialectForm(r.FormValue("reasoning_dialect"), domain.ProtocolOpencode, cur)
	if !ok {
		if state != "" {
			h.sessions.releaseClaim(state)
		}
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	tags, err := domain.ParseTagList(r.FormValue("tags"))
	if err != nil {
		if state != "" {
			h.sessions.releaseClaim(state)
		}
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	cur.Name = r.FormValue("name")
	cur.BaseURL = opencode.GoBaseURL
	cur.Protocol = domain.ProtocolOpencode
	cur.AuthMethod = domain.AuthOAuth
	cur.AuthScheme = domain.AuthBearer
	cur.APIKey = ""
	cur.Tags = tags
	cur.ReasoningDialect = dialect
	if err := opencode.ValidateConsoleImport(cur); err != nil {
		if state != "" {
			h.sessions.releaseClaim(state)
		}
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	if cur.OAuthCreds == nil || !cur.OAuthCreds.OpencodeConsoleAuth {
		if state != "" {
			h.sessions.releaseClaim(state)
		}
		htmxBadRequest(w, r, "provider-flash", "sign in with OpenCode Console before saving")
		return
	}
	if !reconnected {
		err = h.store.UpdateProviderMetaIfConnection(r.Context(), cur.ID, store.ProviderMeta{
			Name:             cur.Name,
			BaseURL:          cur.BaseURL,
			APIKey:           "",
			Protocol:         cur.Protocol,
			AuthScheme:       cur.AuthScheme,
			AuthMethod:       cur.AuthMethod,
			ReasoningDialect: cur.ReasoningDialect,
			Tags:             cur.Tags,
		}, fromMethod, fromProto)
	} else {
		err = h.store.UpdateProviderIfConnection(r.Context(), cur.ID, store.ProviderConnectionSave{
			ProviderMeta: store.ProviderMeta{
				Name:             cur.Name,
				BaseURL:          cur.BaseURL,
				APIKey:           "",
				Protocol:         cur.Protocol,
				AuthScheme:       cur.AuthScheme,
				AuthMethod:       cur.AuthMethod,
				ReasoningDialect: cur.ReasoningDialect,
				Tags:             cur.Tags,
			},
			OAuthCreds: cur.OAuthCreds,
		}, fromMethod, fromProto, basisBlob)
	}
	if err != nil {
		if state != "" {
			h.sessions.releaseClaim(state)
		}
		htmxBadRequest(w, r, "provider-flash", providerSaveErr(err))
		return
	}
	if state != "" {
		h.sessions.drop(state)
	}
	h.renderProviderList(w, r)
}

func (h *Handler) saveOpencodeAPIKey(w http.ResponseWriter, r *http.Request, cur *domain.Provider, fromMethod domain.AuthMethod, fromProto domain.Protocol, basisBlob string) {
	baseURL, apiKey, err := resolveOpencodeCredential(opencodeTierFromForm(r, cur.BaseURL), r.FormValue("api_key"), cur)
	if err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	dialect, ok := parseProviderReasoningDialectForm(r.FormValue("reasoning_dialect"), domain.ProtocolOpencode, cur)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	tags, err := domain.ParseTagList(r.FormValue("tags"))
	if err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	cur.Name = r.FormValue("name")
	cur.BaseURL = baseURL
	cur.Protocol = domain.ProtocolOpencode
	cur.AuthMethod = domain.AuthAPIKey
	cur.AuthScheme = domain.AuthBearer
	cur.APIKey = apiKey
	cur.Tags = tags
	cur.ReasoningDialect = dialect
	hadConsole := cur.OAuthCreds != nil && cur.OAuthCreds.OpencodeConsoleAuth
	cur.OAuthCreds = nil
	if hadConsole || fromMethod == domain.AuthOAuth {
		err = h.store.UpdateProviderIfConnection(r.Context(), cur.ID, store.ProviderConnectionSave{
			ProviderMeta: store.ProviderMeta{
				Name:             cur.Name,
				BaseURL:          cur.BaseURL,
				APIKey:           cur.APIKey,
				Protocol:         cur.Protocol,
				AuthScheme:       cur.AuthScheme,
				AuthMethod:       cur.AuthMethod,
				ReasoningDialect: cur.ReasoningDialect,
				Tags:             cur.Tags,
			},
		}, fromMethod, fromProto, basisBlob)
		if err != nil {
			htmxBadRequest(w, r, "provider-flash", providerSaveErr(err))
			return
		}
		h.renderProviderList(w, r)
		return
	}
	if err := h.store.UpdateProvider(r.Context(), cur); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.renderProviderList(w, r)
}

func (h *Handler) checkOpencodeConsole(w http.ResponseWriter, r *http.Request) {
	targetID, err := opencodeConsoleTarget(r)
	if err != nil {
		render(w, r, CheckResult(false, err.Error()))
		return
	}
	probe, fromStore := h.opencodeConsoleCheckProvider(r, targetID)
	if probe == nil {
		render(w, r, CheckResult(false, "sign in with OpenCode Console before checking"))
		return
	}
	if fromStore {
		resolved, err := h.oauth.ResolveCreds(r.Context(), probe, false)
		switch {
		case oauth.IsInvalidGrant(err):
			render(w, r, CheckResult(false, "reconnect required"))
			return
		case oauth.IsCredentialConflict(err):
			render(w, r, CheckResult(false, "OpenCode Console connection changed; reload and retry"))
			return
		case err != nil:
			render(w, r, CheckResult(false, "OpenCode Console token refresh failed (transient)"))
			return
		}
		probe.OAuthCreds = resolved
	}
	okResult, msg := checkOpencodeConsoleUsage(r.Context(), h.logger, probe)
	render(w, r, CheckResult(okResult, msg))
}

func (h *Handler) opencodeConsoleCheckProvider(r *http.Request, targetID int64) (*domain.Provider, bool) {
	if state := r.FormValue("oauth_session"); state != "" {
		creds, ok := h.sessions.peekOpencodeConsole(state, targetID)
		if !ok {
			return nil, false
		}
		return &domain.Provider{
			BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
			AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: creds,
		}, false
	}
	if targetID == 0 {
		return nil, false
	}
	p, err := h.store.GetProvider(r.Context(), targetID)
	if err != nil || p.Protocol != domain.ProtocolOpencode || p.Method() != domain.AuthOAuth ||
		p.OAuthCreds == nil || !p.OAuthCreds.OpencodeConsoleAuth {
		return nil, false
	}
	return p, true
}

func checkOpencodeConsoleUsage(ctx context.Context, logger *slog.Logger, p *domain.Provider) (bool, string) {
	if err := opencode.RejectUnsafeSend(p, opencode.GoUsageURL); err != nil {
		return false, err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencode.GoUsageURL, nil)
	if err != nil {
		return false, "OpenCode Go usage check failed"
	}
	req.Header.Set("Accept", "application/json")
	opencode.ApplyConsoleAuth(req.Header, p.OAuthCreds)
	client := opencode.ClientNoRedirect(upstreamClient)
	pr, err := executeProbe(ctx, logger, client, req, "check_opencode_console")
	if err != nil {
		return false, "OpenCode Go usage check failed (transient)"
	}
	return interpretOpencodeConsoleUsage(pr.StatusCode, pr.Body, p.OAuthCreds.OpencodeOrgName)
}

func interpretOpencodeConsoleUsage(status int, body []byte, workspace string) (bool, string) {
	label := workspace
	if label == "" {
		label = "the connected workspace"
	}
	switch {
	case status == http.StatusUnauthorized:
		return false, "reconnect required"
	case status == http.StatusForbidden && opencodeSSORequired(body):
		return false, "Sign in with SSO again to use " + label
	case status == http.StatusForbidden:
		return false, "OpenCode Go is not enabled for " + label
	case status >= 500:
		return false, "OpenCode Go usage check failed (transient)"
	case status < 200 || status >= 300:
		return false, "OpenCode Go usage check was rejected"
	}
	var parsed struct {
		Usage struct {
			Rolling struct {
				Status string `json:"status"`
			} `json:"rolling"`
			Weekly struct {
				Status string `json:"status"`
			} `json:"weekly"`
			Monthly struct {
				Status string `json:"status"`
			} `json:"monthly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Usage.Rolling.Status == "" || parsed.Usage.Weekly.Status == "" || parsed.Usage.Monthly.Status == "" {
		return false, "OpenCode Go usage check response was not valid"
	}
	return true, "OK - OpenCode Go Console authorized for " + label
}

func opencodeSSORequired(body []byte) bool {
	var parsed struct {
		Tag string `json:"_tag"`
	}
	return json.Unmarshal(body, &parsed) == nil && parsed.Tag == "SsoRequired"
}

func timeNow() time.Time { return time.Now() }

func opencodeFormTier(p *domain.Provider) string {
	if p == nil || opencode.Tier(p.BaseURL) != "go" {
		return "zen"
	}
	return "go"
}

func opencodeFormAuth(p *domain.Provider) string {
	if p != nil && p.Method() == domain.AuthOAuth && p.OAuthCreds != nil && p.OAuthCreds.OpencodeConsoleAuth && opencodeFormTier(p) == "go" {
		return "console"
	}
	return "apikey"
}

func opencodeFormMethod(p *domain.Provider) string {
	if opencodeFormAuth(p) == "console" {
		return string(domain.AuthOAuth)
	}
	return string(domain.AuthAPIKey)
}

func opencodeWorkspaceLabel(p *domain.Provider) string {
	if p == nil || p.OAuthCreds == nil || p.OAuthCreds.OpencodeOrgName == "" {
		return "OpenCode Console"
	}
	return p.OAuthCreds.OpencodeOrgName
}

type opencodeWorkspaceView struct {
	ID   string
	Name string
}

func (h *Handler) refreshSavedOpencodeConsole(w http.ResponseWriter, r *http.Request, saved *domain.Provider, _ bool) {
	if saved == nil || saved.Method() != domain.AuthOAuth || saved.Protocol != domain.ProtocolOpencode || saved.OAuthCreds == nil || !saved.OAuthCreds.OpencodeConsoleAuth {
		render(w, r, OAuthRefreshResult("", "reconnect required"))
		return
	}
	updated, err := h.oauth.RefreshAndPersist(r.Context(), saved.ID, cloneOAuthCreds(saved.OAuthCreds))
	if err != nil {
		switch {
		case oauth.IsCredentialConflict(err):
			render(w, r, OAuthRefreshResult("", "credentials changed during refresh; reload and retry"))
		case oauth.IsInvalidGrant(err):
			render(w, r, OAuthRefreshResult("", "reconnect required"))
		default:
			render(w, r, OAuthRefreshResult("", "OpenCode Console refresh failed"))
		}
		return
	}
	render(w, r, OAuthRefreshResult(refreshOKMsg(updated), ""))
}
