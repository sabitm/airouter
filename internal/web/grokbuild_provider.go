package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/store"
)

func grokBuildCredential(c *domain.OAuthCreds) bool {
	return c != nil && (c.GrokBuildAuth || c.Preset == grokbuild.Preset)
}

// grokBuildSessionIsolated reports a connect attempt that generic save and
// check must not consume. Bound flow, the subscription marker, and the preset
// are each sufficient. A missing marker does not make a bound attempt generic.
func grokBuildSessionIsolated(sess *connectSession, creds *domain.OAuthCreds) bool {
	if sess != nil && sess.bound && sess.flow == grokBuildFlow {
		return true
	}
	if grokBuildCredential(creds) {
		return true
	}
	if sess == nil || creds != nil {
		return false
	}
	got, _, done := sess.conn.Result()
	return done && grokBuildCredential(got)
}

func grokBuildGenericProbeBlocked(p *domain.Provider) bool {
	if p == nil {
		return false
	}
	if p.Protocol == domain.ProtocolGrokBuild || grokBuildCredential(p.OAuthCreds) {
		return true
	}
	return false
}

func grokBuildTarget(r *http.Request) (int64, error) {
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

func (h *Handler) createGrokBuildProvider(w http.ResponseWriter, r *http.Request) {
	if domain.AuthMethod(r.FormValue("auth_method")) != domain.AuthOAuth || r.FormValue("preset") != grokbuild.Preset {
		htmxBadRequest(w, r, "provider-flash", "Grok Build requires its device login")
		return
	}
	targetID, err := grokBuildTarget(r)
	if err != nil || targetID != 0 {
		htmxBadRequest(w, r, "provider-flash", "Grok Build sign-in does not match this form")
		return
	}
	state := r.FormValue("oauth_session")
	creds, ok := h.sessions.claimGrokBuild(state, 0)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "sign in with Grok Build before saving")
		return
	}
	if err := h.writeNewGrokBuild(r, creds); err != nil {
		h.sessions.releaseClaim(state)
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.sessions.drop(state)
	h.renderProviderList(w, r)
}

func (h *Handler) writeNewGrokBuild(r *http.Request, creds *domain.OAuthCreds) error {
	dialect, ok := parseReasoningDialectForm(r.FormValue("reasoning_dialect"), domain.ProtocolGrokBuild)
	if !ok {
		return errors.New("invalid reasoning dialect")
	}
	tags, err := domain.ParseTagList(r.FormValue("tags"))
	if err != nil {
		return err
	}
	p := &domain.Provider{
		Name:             r.FormValue("name"),
		BaseURL:          grokbuild.BaseURL,
		Protocol:         domain.ProtocolGrokBuild,
		AuthMethod:       domain.AuthOAuth,
		AuthScheme:       domain.AuthBearer,
		OAuthCreds:       creds,
		ReasoningDialect: dialect,
		Tags:             tags,
	}
	if err := grokbuild.ValidateProvider(p); err != nil {
		return err
	}
	return h.store.CreateProvider(r.Context(), p)
}

func (h *Handler) saveGrokBuildProvider(w http.ResponseWriter, r *http.Request, cur *domain.Provider, basisBlob string) {
	if domain.AuthMethod(r.FormValue("auth_method")) != domain.AuthOAuth || r.FormValue("preset") != grokbuild.Preset {
		htmxBadRequest(w, r, "provider-flash", "Grok Build cannot change auth method")
		return
	}
	fromMethod := cur.AuthMethod
	fromProto := cur.Protocol
	state := r.FormValue("oauth_session")
	reconnected := false
	if state != "" {
		creds, ok := h.sessions.claimGrokBuild(state, cur.ID)
		if !ok {
			htmxBadRequest(w, r, "provider-flash", "Grok Build sign-in does not match this provider")
			return
		}
		cur.OAuthCreds = creds
		reconnected = true
	}
	dialect, ok := parseProviderReasoningDialectForm(r.FormValue("reasoning_dialect"), domain.ProtocolGrokBuild, cur)
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
	cur.BaseURL = grokbuild.BaseURL
	cur.Protocol = domain.ProtocolGrokBuild
	cur.AuthMethod = domain.AuthOAuth
	cur.AuthScheme = domain.AuthBearer
	cur.APIKey = ""
	cur.Tags = tags
	if r.Form.Has("reasoning_dialect") || cur.ReasoningDialect == "" {
		cur.ReasoningDialect = dialect
	}
	if err := grokbuild.ValidateProvider(cur); err != nil {
		if state != "" {
			h.sessions.releaseClaim(state)
		}
		htmxBadRequest(w, r, "provider-flash", err.Error())
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
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	if state != "" {
		h.sessions.drop(state)
	}
	h.renderProviderList(w, r)
}

func (h *Handler) checkGrokBuild(w http.ResponseWriter, r *http.Request) {
	targetID, err := grokBuildTarget(r)
	if err != nil {
		render(w, r, CheckResult(false, err.Error()))
		return
	}
	var creds *domain.OAuthCreds
	fromStore := false
	if c, ok := h.sessions.peekGrokBuild(r.FormValue("oauth_session"), targetID); ok {
		creds = c
	} else if targetID > 0 {
		p, err := h.store.GetProvider(r.Context(), targetID)
		if err != nil || p.Protocol != domain.ProtocolGrokBuild || p.OAuthCreds == nil || !p.OAuthCreds.GrokBuildAuth {
			render(w, r, CheckResult(false, "not connected yet - run Connect first"))
			return
		}
		creds = p.OAuthCreds
		fromStore = true
	}
	if creds == nil {
		render(w, r, CheckResult(false, "not connected yet - run Connect first"))
		return
	}
	probe := &domain.Provider{
		ID: targetID, BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: creds,
	}
	if fromStore {
		resolved, err := h.oauth.ResolveCreds(r.Context(), probe, false)
		if err != nil {
			if oauth.IsInvalidGrant(err) {
				render(w, r, CheckResult(false, "token expired - reconnect required"))
				return
			}
			render(w, r, CheckResult(false, "token refresh failed"))
			return
		}
		if resolved != nil {
			cp := *resolved
			probe.OAuthCreds = &cp
			probe.APIKey = cp.AccessToken
		}
	} else {
		probe.APIKey = creds.AccessToken
	}
	if err := grokbuild.ValidateCredentialUse(probe); err != nil {
		render(w, r, CheckResult(false, err.Error()))
		return
	}
	ok, msg := checkGrokBuildUpstream(r.Context(), probe)
	render(w, r, CheckResult(ok, msg))
}

func (h *Handler) refreshSavedGrokBuild(w http.ResponseWriter, r *http.Request, saved *domain.Provider) {
	if saved == nil || saved.Method() != domain.AuthOAuth || saved.Protocol != domain.ProtocolGrokBuild || !grokBuildCredential(saved.OAuthCreds) || saved.OAuthCreds == nil || !saved.OAuthCreds.GrokBuildAuth {
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
			render(w, r, OAuthRefreshResult("", "Grok Build refresh failed"))
		}
		return
	}
	render(w, r, OAuthRefreshResult(refreshOKMsg(updated), ""))
}

func checkGrokBuildUpstream(ctx context.Context, p *domain.Provider) (bool, string) {
	if err := grokbuild.RejectUnsafeSend(p, grokbuild.ModelsURL); err != nil {
		return false, err.Error()
	}
	userID := ""
	if p.OAuthCreds != nil {
		userID = p.OAuthCreds.UserID
	}
	models, err := grokbuild.ListModels(ctx, p.APIKey, userID)
	if err != nil {
		return false, err.Error()
	}
	return true, strconv.Itoa(len(models)) + " models"
}
