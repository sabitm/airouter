package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/store"
)

const (
	grokBuildModeField  = "grok_build_mode"
	grokBuildModeDevice = "device"
	grokBuildModeManual = "manual"
	// grokBuildExpiryMax is 9999-12-31T23:59:59Z. The explicit form value must
	// not silently wrap into a different date.
	grokBuildExpiryMax = int64(253402300799)
)

// grokBuildFormInput is one create, edit, or check submission. manual is nil
// for device sign-in, a metadata-only edit, or an all-blank manual form. A
// manual pair never carries a device session.
type grokBuildFormInput struct {
	session string
	manual  *domain.OAuthCreds
	// manualBlank reports an explicit manual form with no nonblank token value.
	// Create and Check reject it; an edit keeps the stored login.
	manualBlank bool
}

var grokBuildManualFields = []string{"access_token", "refresh_token", "id_token", "expires_at"}

// parseGrokBuildForm separates device sign-in from a pasted token pair before
// any connect session is claimed. A form without a mode is a legacy device
// form. Any manual field without manual mode is rejected so saved credentials
// are never substituted for an attempted import.
func parseGrokBuildForm(r *http.Request) (*grokBuildFormInput, error) {
	if r == nil {
		return nil, errors.New("invalid form")
	}
	modes := r.Form[grokBuildModeField]
	if len(modes) > 1 {
		return nil, errors.New("Grok Build sign-in mode is invalid")
	}
	mode := grokBuildModeDevice
	explicitMode := false
	if len(modes) == 1 {
		explicitMode = true
		switch strings.TrimSpace(modes[0]) {
		case grokBuildModeDevice:
		case grokBuildModeManual:
			mode = grokBuildModeManual
		default:
			return nil, errors.New("Grok Build sign-in mode is invalid")
		}
	}
	values, manualFields, err := grokBuildManualValues(r)
	if err != nil {
		return nil, err
	}
	if mode == grokBuildModeManual {
		for _, session := range r.Form["oauth_session"] {
			if strings.TrimSpace(session) != "" {
				return nil, errors.New("Grok Build device sign-in and pasted tokens cannot be combined")
			}
		}
		if !grokBuildManualNonblank(values) {
			return &grokBuildFormInput{manualBlank: true}, nil
		}
		expiresAt, err := parseGrokBuildExpiry(values["expires_at"])
		if err != nil {
			return nil, err
		}
		creds, err := oauth.GrokBuildManualCreds(values["access_token"], values["refresh_token"], values["id_token"], expiresAt)
		if err != nil {
			return nil, err
		}
		return &grokBuildFormInput{manual: creds}, nil
	}
	if manualFields {
		if explicitMode {
			return nil, errors.New("Grok Build device sign-in and pasted tokens cannot be combined")
		}
		return nil, errors.New("Grok Build sign-in mode is required for pasted tokens")
	}
	return &grokBuildFormInput{session: r.FormValue("oauth_session")}, nil
}

// grokBuildManualValues rejects duplicate submitted token fields. Duplicates
// are material because Go's FormValue would silently choose the first value.
func grokBuildManualValues(r *http.Request) (map[string]string, bool, error) {
	values := make(map[string]string, len(grokBuildManualFields))
	present := false
	for _, name := range grokBuildManualFields {
		submitted, ok := r.Form[name]
		if !ok {
			continue
		}
		present = true
		if len(submitted) != 1 {
			return nil, true, errors.New("duplicate Grok Build token field")
		}
		values[name] = submitted[0]
	}
	return values, present, nil
}

func grokBuildManualNonblank(values map[string]string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// parseGrokBuildExpiry accepts only an explicit RFC3339 timestamp or unix
// seconds. An empty value means unknown and may use a JWT exp hint later. A
// malformed or out-of-range explicit value is rejected instead of ignored.
func parseGrokBuildExpiry(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	if len(value) > 64 {
		return 0, errors.New("Grok Build expiry is invalid")
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		if unix <= 0 || unix > grokBuildExpiryMax {
			return 0, errors.New("Grok Build expiry is invalid")
		}
		return unix, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		unix := parsed.Unix()
		if unix <= 0 || unix > grokBuildExpiryMax {
			return 0, errors.New("Grok Build expiry is invalid")
		}
		return unix, nil
	}
	return 0, errors.New("Grok Build expiry must be RFC3339 or unix seconds")
}

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
		htmxBadRequest(w, r, "provider-flash", "Grok Build requires OAuth sign-in")
		return
	}
	targetID, err := grokBuildTarget(r)
	if err != nil || targetID != 0 {
		htmxBadRequest(w, r, "provider-flash", "Grok Build sign-in does not match this form")
		return
	}
	input, err := parseGrokBuildForm(r)
	if err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	if input.manualBlank {
		htmxBadRequest(w, r, "provider-flash", "paste both an access token and a refresh token")
		return
	}
	creds := input.manual
	if creds == nil {
		var ok bool
		creds, ok = h.sessions.claimGrokBuild(input.session, 0)
		if !ok {
			htmxBadRequest(w, r, "provider-flash", "sign in with Grok Build before saving")
			return
		}
	}
	if err := h.writeNewGrokBuild(r, creds); err != nil {
		if input.manual == nil {
			h.sessions.releaseClaim(input.session)
		}
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	if input.manual == nil {
		h.sessions.drop(input.session)
	}
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
	input, err := parseGrokBuildForm(r)
	if err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	fromMethod := cur.AuthMethod
	fromProto := cur.Protocol
	state := input.session
	claimed := false
	reconnected := false
	if input.manual != nil {
		cur.OAuthCreds = input.manual
		reconnected = true
	} else if state != "" {
		creds, ok := h.sessions.claimGrokBuild(state, cur.ID)
		if !ok {
			htmxBadRequest(w, r, "provider-flash", "Grok Build sign-in does not match this provider")
			return
		}
		cur.OAuthCreds = creds
		claimed = true
		reconnected = true
	}
	releaseClaim := func() {
		if claimed {
			h.sessions.releaseClaim(state)
			claimed = false
		}
	}
	dialect, ok := parseProviderReasoningDialectForm(r.FormValue("reasoning_dialect"), domain.ProtocolGrokBuild, cur)
	if !ok {
		releaseClaim()
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	tags, err := domain.ParseTagList(r.FormValue("tags"))
	if err != nil {
		releaseClaim()
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
		releaseClaim()
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
		releaseClaim()
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	if claimed {
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
	input, err := parseGrokBuildForm(r)
	if err != nil {
		render(w, r, CheckResult(false, err.Error()))
		return
	}
	if input.manualBlank {
		// A blank manual form is not permission to check the saved row.
		render(w, r, CheckResult(false, "paste both an access token and a refresh token"))
		return
	}
	if input.manual != nil {
		// A candidate check sends exactly the submitted access token. It never
		// refreshes, writes, or falls back to a saved row or device session.
		probe := &domain.Provider{
			ID: targetID, BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
			AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
			OAuthCreds: input.manual, APIKey: input.manual.AccessToken,
		}
		if err := grokbuild.ValidateCredentialUse(probe); err != nil {
			render(w, r, CheckResult(false, err.Error()))
			return
		}
		ok, msg := checkGrokBuildUpstream(r.Context(), probe)
		render(w, r, CheckResult(ok, msg))
		return
	}
	var creds *domain.OAuthCreds
	fromStore := false
	if c, ok := h.sessions.peekGrokBuild(input.session, targetID); ok {
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
		case oauth.IsGrokBuildAccountMismatch(err):
			render(w, r, OAuthRefreshResult("", "pasted tokens belong to different accounts; reconnect required"))
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
