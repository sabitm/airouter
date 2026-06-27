package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/store"
)

type Handler struct {
	store *store.Store
	// oauth resolves an effective token for oauth providers before the dashboard
	// probes an upstream (Check button, model autocomplete).
	oauth *oauth.Service
	// sessions holds in-flight OAuth connect attempts between the begin request
	// and the later status/exchange/save requests.
	sessions *connectSessions
	// trace, set at -debug=2, logs the dashboard's outbound provider subcalls
	// (e.g. the /models probe behind the Check button) that the request-logging
	// middleware cannot see.
	trace bool
}

func NewHandler(s *store.Store, trace bool) *Handler {
	return &Handler{store: s, oauth: oauth.New(s), sessions: newConnectSessions(), trace: trace}
}

// Mount registers all dashboard routes on the given mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(StaticFS()))))

	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/providers", http.StatusFound)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/dashboard/providers", http.StatusFound)
	})

	// Providers
	mux.HandleFunc("GET /dashboard/providers", h.providersPage)
	mux.HandleFunc("POST /dashboard/providers", h.createProvider)
	mux.HandleFunc("GET /dashboard/providers/{id}/edit", h.editProvider)
	mux.HandleFunc("GET /dashboard/providers/{id}/row", h.providerRow)
	mux.HandleFunc("POST /dashboard/providers/{id}", h.updateProvider)
	mux.HandleFunc("POST /dashboard/providers/{id}/delete", h.deleteProvider)

	mux.HandleFunc("GET /dashboard/providers/models", h.providerModels)
	mux.HandleFunc("POST /dashboard/providers/check", h.checkProvider)

	// OAuth connect flow
	mux.HandleFunc("POST /dashboard/providers/oauth/begin", h.beginOAuthConnect)
	mux.HandleFunc("GET /dashboard/providers/oauth/status", h.oauthConnectStatus)
	mux.HandleFunc("POST /dashboard/providers/oauth/exchange", h.oauthConnectExchange)
	mux.HandleFunc("POST /dashboard/providers/oauth/cancel", h.oauthConnectCancel)
	mux.HandleFunc("POST /dashboard/providers/oauth/refresh", h.oauthRefreshTokens)
	mux.HandleFunc("POST /dashboard/providers/oauth/refresh-all", h.refreshAllOAuth)

	// Combos
	mux.HandleFunc("GET /dashboard/combos", h.combosPage)
	mux.HandleFunc("POST /dashboard/combos", h.createCombo)
	mux.HandleFunc("GET /dashboard/combos/{id}/edit", h.editCombo)
	mux.HandleFunc("GET /dashboard/combos/{id}/row", h.comboRow)
	mux.HandleFunc("POST /dashboard/combos/{id}", h.updateCombo)
	mux.HandleFunc("POST /dashboard/combos/{id}/delete", h.deleteCombo)

	// Access keys
	mux.HandleFunc("GET /dashboard/keys", h.keysPage)
	mux.HandleFunc("POST /dashboard/keys", h.createKey)
	mux.HandleFunc("POST /dashboard/keys/{id}/delete", h.deleteKey)

	// Logs
	mux.HandleFunc("GET /dashboard/logs", h.logsPage)
	mux.HandleFunc("POST /dashboard/logs/clear", h.clearLogs)

	// Settings
	mux.HandleFunc("GET /dashboard/settings", h.settingsPage)
	mux.HandleFunc("GET /dashboard/export", h.exportConfig)
	mux.HandleFunc("POST /dashboard/import", h.importConfig)
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// --- Providers ---

func (h *Handler) providersPage(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, ProvidersPage(providers, routerBaseURL(r)))
}

// routerBaseURL reconstructs the router's externally visible origin from the
// request, honoring X-Forwarded-Proto when behind a TLS-terminating proxy.
func routerBaseURL(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *Handler) createProvider(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		badRequest(w, "invalid form")
		return
	}
	proto := domain.Protocol(r.FormValue("protocol"))
	if !proto.Valid() {
		badRequest(w, "invalid protocol")
		return
	}
	if domain.AuthMethod(r.FormValue("auth_method")) == domain.AuthOAuth {
		h.createOAuthProvider(w, r, proto)
		return
	}
	auth := domain.AuthScheme(r.FormValue("auth_scheme"))
	if auth != "" && !auth.Valid() {
		badRequest(w, "invalid auth scheme")
		return
	}
	p := &domain.Provider{
		Name:       r.FormValue("name"),
		BaseURL:    r.FormValue("base_url"),
		APIKey:     r.FormValue("api_key"),
		Protocol:   proto,
		AuthScheme: auth,
	}
	// "default" (empty auth) is an alias: expand it now to the protocol's scheme
	// so the stored value is always concrete.
	p.AuthScheme = p.Auth()
	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderProviderList(w, r)
}

// createOAuthProvider saves an oauth provider. Credentials come from one of two
// sources, in order: a completed connect session (keyed by oauth_session = the
// connect state), or tokens pasted into the form (importing an already-
// authenticated session, with config from the preset/manual fields). An oauth
// provider stores no static key and always authenticates with a bearer token.
func (h *Handler) createOAuthProvider(w http.ResponseWriter, r *http.Request, proto domain.Protocol) {
	creds, ok := h.connectedCreds(r.FormValue("oauth_session"))
	if !ok {
		c, err := credsFromConnectForm(r)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		if !applyManualTokens(c, r) {
			badRequest(w, "connect this provider or paste an access/refresh token before saving")
			return
		}
		creds = c
	}
	p := &domain.Provider{
		Name:       r.FormValue("name"),
		BaseURL:    r.FormValue("base_url"),
		Protocol:   proto,
		AuthMethod: domain.AuthOAuth,
		AuthScheme: domain.AuthBearer,
		OAuthCreds: creds,
	}
	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.sessions.drop(r.FormValue("oauth_session"))
	h.renderProviderList(w, r)
}

// connectedCreds returns the completed credentials for a connect session, or
// false if the session is unknown or the flow has not completed successfully.
func (h *Handler) connectedCreds(state string) (*domain.OAuthCreds, bool) {
	if state == "" {
		return nil, false
	}
	sess, ok := h.sessions.get(state)
	if !ok {
		return nil, false
	}
	creds, err, done := sess.conn.Result()
	if !done || err != nil || creds == nil || creds.AccessToken == "" {
		return nil, false
	}
	return creds, true
}

func (h *Handler) editProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, r, ProviderEditRow(p))
}

func (h *Handler) providerRow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, r, ProviderRow(p))
}

func (h *Handler) updateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	if err := r.ParseForm(); err != nil {
		badRequest(w, "invalid form")
		return
	}
	cur, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	proto := domain.Protocol(r.FormValue("protocol"))
	if !proto.Valid() {
		badRequest(w, "invalid protocol")
		return
	}
	if domain.AuthMethod(r.FormValue("auth_method")) == domain.AuthOAuth {
		h.updateOAuthProvider(w, r, cur, proto)
		return
	}
	auth := domain.AuthScheme(r.FormValue("auth_scheme"))
	if auth != "" && !auth.Valid() {
		badRequest(w, "invalid auth scheme")
		return
	}
	cur.Name = r.FormValue("name")
	cur.BaseURL = r.FormValue("base_url")
	cur.Protocol = proto
	cur.AuthScheme = auth
	// Switching an oauth provider back to apikey: drop the stored credentials so
	// the row no longer resolves a bearer token.
	cur.AuthMethod = domain.AuthAPIKey
	cur.OAuthCreds = nil
	// Blank api_key means keep the existing one (form never echoes secrets).
	if k := r.FormValue("api_key"); k != "" {
		cur.APIKey = k
	}
	if err := h.store.UpdateProvider(r.Context(), cur); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderProviderList(w, r)
}

// updateOAuthProvider saves edits to an oauth provider. Name/base URL/protocol
// come from the form; credentials are replaced only when a fresh connect session
// (a Reconnect) is attached or fresh tokens are pasted, otherwise the stored
// tokens are kept. The paste fields are blank by default, so editing an oauth
// provider without reconnecting or pasting never requires re-auth.
func (h *Handler) updateOAuthProvider(w http.ResponseWriter, r *http.Request, cur *domain.Provider, proto domain.Protocol) {
	if creds, ok := h.connectedCreds(r.FormValue("oauth_session")); ok {
		cur.OAuthCreds = creds
	} else if c, err := credsFromConnectForm(r); err == nil && applyManualTokens(c, r) {
		cur.OAuthCreds = c
	}
	if cur.OAuthCreds == nil {
		badRequest(w, "connect this provider or paste an access/refresh token before saving")
		return
	}
	cur.Name = r.FormValue("name")
	cur.BaseURL = r.FormValue("base_url")
	cur.Protocol = proto
	cur.AuthMethod = domain.AuthOAuth
	cur.AuthScheme = domain.AuthBearer
	cur.APIKey = ""
	if err := h.store.UpdateProvider(r.Context(), cur); err != nil {
		badRequest(w, err.Error())
		return
	}
	if s := r.FormValue("oauth_session"); s != "" {
		h.sessions.drop(s)
	}
	h.renderProviderList(w, r)
}

func (h *Handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	if err := h.store.DeleteProvider(r.Context(), id); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderProviderList(w, r)
}

// refreshAllOAuth force-refreshes every saved oauth provider's access token,
// persisting the rotated tokens, and reports a one-line summary. Failures are
// collected per provider rather than aborting the batch, so one revoked refresh
// token does not block refreshing the rest.
func (h *Handler) refreshAllOAuth(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		render(w, r, CheckResult(false, err.Error()))
		return
	}
	var refreshed, failed int
	var problems []string
	for _, p := range providers {
		if p.Method() != domain.AuthOAuth {
			continue
		}
		if _, err := h.oauth.Resolve(r.Context(), p, true); err != nil {
			failed++
			if oauth.IsInvalidGrant(err) {
				problems = append(problems, p.Name+": reconnect required")
			} else {
				problems = append(problems, p.Name+": "+err.Error())
			}
			continue
		}
		refreshed++
	}
	if failed == 0 {
		render(w, r, CheckResult(true, fmt.Sprintf("refreshed %d oauth provider(s)", refreshed)))
		return
	}
	render(w, r, CheckResult(false, fmt.Sprintf("refreshed %d, %d failed: %s", refreshed, failed, strings.Join(problems, "; "))))
}

func (h *Handler) renderProviderList(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, ProviderList(providers))
}

// --- Combos ---

func (h *Handler) combosPage(w http.ResponseWriter, r *http.Request) {
	combos, providers, err := h.comboData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, CombosPage(combos, providers))
}

func (h *Handler) comboData(ctx context.Context) ([]*domain.Combo, []*domain.Provider, error) {
	combos, err := h.store.ListCombos(ctx)
	if err != nil {
		return nil, nil, err
	}
	providers, err := h.store.ListProviders(ctx)
	if err != nil {
		return nil, nil, err
	}
	return combos, providers, nil
}

func (h *Handler) createCombo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		badRequest(w, "invalid form")
		return
	}
	c, err := parseComboForm(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := h.store.CreateCombo(r.Context(), c); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderComboList(w, r)
}

// parseComboForm builds a combo from the form's strategy plus the parallel
// provider_id / upstream_model arrays (one pair per target row). Rows missing a
// provider or model are skipped; at least one complete target is required.
func parseComboForm(r *http.Request) (*domain.Combo, error) {
	providerIDs := r.Form["provider_id"]
	models := r.Form["upstream_model"]
	var targets []domain.ComboTarget
	for i, raw := range providerIDs {
		var model string
		if i < len(models) {
			model = strings.TrimSpace(models[i])
		}
		raw = strings.TrimSpace(raw)
		if raw == "" || model == "" {
			continue
		}
		pid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid provider in target %d", i+1)
		}
		targets = append(targets, domain.ComboTarget{ProviderID: pid, UpstreamModel: model})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("a combo needs at least one provider + model target")
	}
	strategy := domain.ComboStrategy(r.FormValue("strategy"))
	if strategy == "" {
		strategy = domain.StrategyFailover
	}
	if !strategy.Valid() {
		return nil, fmt.Errorf("invalid strategy %q", r.FormValue("strategy"))
	}
	return &domain.Combo{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Strategy: strategy,
		Targets:  targets,
	}, nil
}

func (h *Handler) editCombo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	c, err := h.store.GetCombo(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, ComboEditRow(c, providers))
}

func (h *Handler) comboRow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	// ListCombos hydrates the provider needed by ComboRow; fetch and find.
	combos, err := h.store.ListCombos(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for _, c := range combos {
		if c.ID == id {
			render(w, r, ComboRow(c))
			return
		}
	}
	http.NotFound(w, r)
}

func (h *Handler) updateCombo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	if err := r.ParseForm(); err != nil {
		badRequest(w, "invalid form")
		return
	}
	c, err := parseComboForm(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	c.ID = id
	if err := h.store.UpdateCombo(r.Context(), c); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderComboList(w, r)
}

func (h *Handler) deleteCombo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	if err := h.store.DeleteCombo(r.Context(), id); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderComboList(w, r)
}

func (h *Handler) renderComboList(w http.ResponseWriter, r *http.Request) {
	combos, err := h.store.ListCombos(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, ComboList(combos))
}

// --- Access keys ---

func (h *Handler) keysPage(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.ListAccessKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, KeysPage(keys))
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		badRequest(w, "invalid form")
		return
	}
	created, err := h.store.NewAccessKey(r.Context(), r.FormValue("name"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	keys, err := h.store.ListAccessKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, KeyList(keys, created))
}

func (h *Handler) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		badRequest(w, "bad id")
		return
	}
	if err := h.store.DeleteAccessKey(r.Context(), id); err != nil {
		badRequest(w, err.Error())
		return
	}
	keys, err := h.store.ListAccessKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, KeyList(keys, nil))
}

// --- Logs ---

const logsPageLimit = 200

func (h *Handler) logsPage(w http.ResponseWriter, r *http.Request) {
	logs, err := h.store.ListRequestLogs(r.Context(), logsPageLimit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	reqs, in, out, err := h.store.RequestLogStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, LogsPage(logs, LogStats{TotalReqs: reqs, TotalIn: in, TotalOut: out}))
}

func (h *Handler) clearLogs(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ClearRequestLogs(r.Context()); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, LogsBody(nil, LogStats{}))
}

// --- Settings / import-export ---

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, SettingsPage())
}

func (h *Handler) exportConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="airouter-config.json"`)
	if err := h.store.Export(r.Context(), w); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (h *Handler) importConfig(w http.ResponseWriter, r *http.Request) {
	file, _, err := r.FormFile("config")
	if err != nil {
		render(w, r, flash("error", "no file uploaded"))
		return
	}
	defer file.Close()
	if err := h.store.Import(r.Context(), file); err != nil {
		render(w, r, flash("error", "import failed: "+err.Error()))
		return
	}
	render(w, r, flash("ok", "import complete"))
}
