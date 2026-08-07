package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"airouter/internal/domain"
	"airouter/internal/harlog"
	"airouter/internal/oauth"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/qoder"
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
	// logger is the component=web logger. TRACE enables outbound probe exchange metadata.
	logger *slog.Logger
	// har is the process-wide capture controller shared with server middleware.
	har *harlog.Controller
}

// NewHandler builds the dashboard handler. logger may be nil (falls back to
// slog.Default). Prefer a component=web logger from the server constructor.
// har may be nil (HAR panel renders as disabled/idle).
func NewHandler(s *store.Store, logger *slog.Logger, har *harlog.Controller) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{store: s, oauth: oauth.New(s), sessions: newConnectSessions(), logger: logger, har: har}
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
	mux.HandleFunc("GET /dashboard/providers/new", h.newProviderForm)
	mux.HandleFunc("POST /dashboard/providers", h.createProvider)
	mux.HandleFunc("GET /dashboard/providers/{id}/edit", h.editProvider)
	mux.HandleFunc("GET /dashboard/providers/{id}/row", h.providerRow)
	mux.HandleFunc("POST /dashboard/providers/{id}", h.updateProvider)
	mux.HandleFunc("POST /dashboard/providers/{id}/delete", h.deleteProvider)
	mux.HandleFunc("POST /dashboard/providers/{id}/archive", h.archiveProvider)
	mux.HandleFunc("POST /dashboard/providers/{id}/restore", h.restoreProvider)
	mux.HandleFunc("POST /dashboard/providers/archived/delete-all", h.deleteAllArchived)

	mux.HandleFunc("GET /dashboard/providers/models", h.providerModels)
	mux.HandleFunc("POST /dashboard/providers/check", h.checkProvider)

	// OAuth connect flow
	mux.HandleFunc("POST /dashboard/providers/oauth/begin", h.beginOAuthConnect)
	mux.HandleFunc("GET /dashboard/providers/oauth/status", h.oauthConnectStatus)
	mux.HandleFunc("POST /dashboard/providers/oauth/exchange", h.oauthConnectExchange)
	mux.HandleFunc("POST /dashboard/providers/oauth/cancel", h.oauthConnectCancel)
	mux.HandleFunc("POST /dashboard/providers/oauth/refresh", h.oauthRefreshTokens)
	mux.HandleFunc("POST /dashboard/providers/oauth/refresh-all", h.refreshAllOAuth)
	mux.HandleFunc("POST /dashboard/providers/kiro/device/begin", h.kiroDeviceBegin)
	mux.HandleFunc("POST /dashboard/providers/qoder/device/begin", h.qoderDeviceBegin)

	// Combos
	mux.HandleFunc("GET /dashboard/combos", h.combosPage)
	mux.HandleFunc("POST /dashboard/combos", h.createCombo)
	mux.HandleFunc("GET /dashboard/combos/{id}/edit", h.editCombo)
	mux.HandleFunc("GET /dashboard/combos/{id}/row", h.comboRow)
	mux.HandleFunc("POST /dashboard/combos/{id}", h.updateCombo)
	mux.HandleFunc("POST /dashboard/combos/{id}/delete", h.deleteCombo)
	mux.HandleFunc("POST /dashboard/combos/{id}/targets/{tid}/toggle", h.toggleComboTarget)
	mux.HandleFunc("GET /dashboard/combos/{id}/swap", h.swapComboForm)
	mux.HandleFunc("POST /dashboard/combos/{id}/swap", h.swapCombo)

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
	mux.HandleFunc("GET /dashboard/har/status", h.harStatus)
	mux.HandleFunc("POST /dashboard/har/start", h.harStart)
	mux.HandleFunc("POST /dashboard/har/stop", h.harStop)
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

// htmxBadRequest returns a styled flash for HTMX mutations, or a plain HTTP
// error for non-HTMX callers. flashID is the sink element to retarget when the
// request was aimed at a list/row that should stay intact on validation failure.
func htmxBadRequest(w http.ResponseWriter, r *http.Request, flashID, msg string) {
	if r.Header.Get("HX-Request") != "" {
		renderHXFlash(w, r, flashID, msg)
		return
	}
	badRequest(w, msg)
}

func renderHXFlash(w http.ResponseWriter, r *http.Request, flashID, msg string) {
	if flashID != "" {
		w.Header().Set("HX-Retarget", "#"+flashID)
		w.Header().Set("HX-Reswap", "innerHTML")
	}
	// 400 so hx-on::after-request success handlers (form close/reset) do not fire;
	// dashboard.js allows HTMX to still swap 400 bodies into the flash sink.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	if err := flash("error", msg).Render(r.Context(), w); err != nil {
		// headers already committed
	}
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

func (h *Handler) newProviderForm(w http.ResponseWriter, r *http.Request) {
	rec, ok := recipeByID(r.URL.Query().Get("recipe"))
	if !ok {
		badRequest(w, "unknown provider type")
		return
	}
	render(w, r, ProviderRecipeForm(rec))
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

// kiroBaseURLOr defaults a blank Kiro base URL to the CodeWhisperer host so the
// user need not memorize it; non-Kiro or non-blank values pass through.
func kiroBaseURLOr(proto domain.Protocol, base string) string {
	if proto == domain.ProtocolKiro && strings.TrimSpace(base) == "" {
		return kiro.DefaultBaseURL
	}
	if proto == domain.ProtocolQoder && strings.TrimSpace(base) == "" {
		return qoder.DefaultBaseURL
	}
	if proto == domain.ProtocolAntigravity && strings.TrimSpace(base) == "" {
		return antigravity.DefaultBaseURL
	}
	if proto == domain.ProtocolCursor && strings.TrimSpace(base) == "" {
		return cursor.DefaultBaseURL
	}
	if proto == domain.ProtocolClaudeCode && strings.TrimSpace(base) == "" {
		return claudecode.DefaultBaseURL
	}
	return base
}

func (h *Handler) createProvider(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		htmxBadRequest(w, r, "provider-flash", "invalid form")
		return
	}
	proto := domain.Protocol(r.FormValue("protocol"))
	if !proto.Valid() {
		htmxBadRequest(w, r, "provider-flash", "invalid protocol")
		return
	}
	if domain.AuthMethod(r.FormValue("auth_method")) == domain.AuthOAuth {
		h.createOAuthProvider(w, r, proto)
		return
	}
	auth := domain.AuthScheme(r.FormValue("auth_scheme"))
	if auth != "" && !auth.Valid() {
		htmxBadRequest(w, r, "provider-flash", "invalid auth scheme")
		return
	}
	apiKey := strings.TrimSpace(r.FormValue("api_key"))
	// Generic apikey providers need a credential at create time. Kiro may still
	// rely on profile/oauth paths and is validated separately there.
	if proto != domain.ProtocolKiro && apiKey == "" {
		htmxBadRequest(w, r, "provider-flash", "API key is required")
		return
	}
	dialect, ok := parseReasoningDialectForm(r.FormValue("reasoning_dialect"), proto)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	p := &domain.Provider{
		Name:             r.FormValue("name"),
		BaseURL:          kiroBaseURLOr(proto, r.FormValue("base_url")),
		APIKey:           apiKey,
		Protocol:         proto,
		AuthScheme:       auth,
		ReasoningDialect: dialect,
	}
	// "default" (empty auth) is an alias: expand it now to the protocol's scheme
	// so the stored value is always concrete.
	p.AuthScheme = p.Auth()
	// A Kiro apikey provider still needs its profile ARN/region; carry them on a
	// token-less OAuthCreds so the request encoder reads profile config uniformly.
	if proto == domain.ProtocolKiro {
		creds := &domain.OAuthCreds{}
		applyKiroConfig(creds, r)
		p.OAuthCreds = creds
	}
	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
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
			htmxBadRequest(w, r, "provider-flash", err.Error())
			return
		}
		if !applyManualTokens(c, r) {
			htmxBadRequest(w, r, "provider-flash", "connect this provider or paste an access/refresh token before saving")
			return
		}
		creds = c
	}
	// Kiro oauth providers carry the profile ARN/region and the auth-flavor marker
	// that routes token refresh to Kiro's flow.
	if proto == domain.ProtocolKiro {
		applyKiroConfig(creds, r)
	}
	if proto == domain.ProtocolQoder {
		applyQoderConfig(creds, r)
	}
	if proto == domain.ProtocolAntigravity {
		applyAntigravityConfig(creds, r)
		if err := oauth.EnsureAntigravityProject(r.Context(), creds); err != nil {
			htmxBadRequest(w, r, "provider-flash", err.Error())
			return
		}
		if strings.TrimSpace(creds.ProjectID) == "" {
			htmxBadRequest(w, r, "provider-flash", "antigravity: missing project id; reconnect OAuth")
			return
		}
	}
	if proto == domain.ProtocolCursor {
		applyCursorConfig(creds, r)
		if strings.TrimSpace(creds.AccessToken) == "" {
			htmxBadRequest(w, r, "provider-flash", "cursor: paste an access token before saving")
			return
		}
		if strings.TrimSpace(creds.MachineID) == "" {
			htmxBadRequest(w, r, "provider-flash", "cursor: paste the machine id before saving")
			return
		}
	}
	dialect, ok := parseReasoningDialectForm(r.FormValue("reasoning_dialect"), proto)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	p := &domain.Provider{
		Name:             r.FormValue("name"),
		BaseURL:          kiroBaseURLOr(proto, r.FormValue("base_url")),
		Protocol:         proto,
		AuthMethod:       domain.AuthOAuth,
		AuthScheme:       domain.AuthBearer,
		OAuthCreds:       creds,
		ReasoningDialect: dialect,
	}
	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
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
		htmxBadRequest(w, r, "provider-flash", "bad id")
		return
	}
	if err := r.ParseForm(); err != nil {
		htmxBadRequest(w, r, "provider-flash", "invalid form")
		return
	}
	cur, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	proto := domain.Protocol(r.FormValue("protocol"))
	if !proto.Valid() {
		htmxBadRequest(w, r, "provider-flash", "invalid protocol")
		return
	}
	if domain.AuthMethod(r.FormValue("auth_method")) == domain.AuthOAuth {
		h.updateOAuthProvider(w, r, cur, proto)
		return
	}
	auth := domain.AuthScheme(r.FormValue("auth_scheme"))
	if auth != "" && !auth.Valid() {
		htmxBadRequest(w, r, "provider-flash", "invalid auth scheme")
		return
	}
	dialect, ok := parseReasoningDialectForm(r.FormValue("reasoning_dialect"), proto)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	cur.Name = r.FormValue("name")
	cur.BaseURL = kiroBaseURLOr(proto, r.FormValue("base_url"))
	cur.Protocol = proto
	cur.AuthScheme = auth
	cur.ReasoningDialect = dialect
	// Switching an oauth provider back to apikey: drop the stored credentials so
	// the row no longer resolves a bearer token.
	cur.AuthMethod = domain.AuthAPIKey
	cur.OAuthCreds = nil
	// A Kiro apikey provider keeps a token-less OAuthCreds for its profile config.
	if proto == domain.ProtocolKiro {
		creds := &domain.OAuthCreds{}
		applyKiroConfig(creds, r)
		cur.OAuthCreds = creds
	}
	// Blank api_key means keep the existing one (form never echoes secrets).
	if k := r.FormValue("api_key"); k != "" {
		cur.APIKey = k
	}
	if err := h.store.UpdateProvider(r.Context(), cur); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
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
		htmxBadRequest(w, r, "provider-flash", "connect this provider or paste an access/refresh token before saving")
		return
	}
	if proto == domain.ProtocolKiro {
		applyKiroConfig(cur.OAuthCreds, r)
	}
	if proto == domain.ProtocolQoder {
		applyQoderConfig(cur.OAuthCreds, r)
	}
	if proto == domain.ProtocolAntigravity {
		applyAntigravityConfig(cur.OAuthCreds, r)
		if err := oauth.EnsureAntigravityProject(r.Context(), cur.OAuthCreds); err != nil {
			htmxBadRequest(w, r, "provider-flash", err.Error())
			return
		}
		if strings.TrimSpace(cur.OAuthCreds.ProjectID) == "" {
			htmxBadRequest(w, r, "provider-flash", "antigravity: missing project id; reconnect OAuth")
			return
		}
	}
	if proto == domain.ProtocolCursor {
		applyCursorConfig(cur.OAuthCreds, r)
		if strings.TrimSpace(cur.OAuthCreds.AccessToken) == "" {
			htmxBadRequest(w, r, "provider-flash", "cursor: paste an access token before saving")
			return
		}
		if strings.TrimSpace(cur.OAuthCreds.MachineID) == "" {
			htmxBadRequest(w, r, "provider-flash", "cursor: paste the machine id before saving")
			return
		}
	}
	dialect, ok := parseReasoningDialectForm(r.FormValue("reasoning_dialect"), proto)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "invalid reasoning dialect")
		return
	}
	cur.Name = r.FormValue("name")
	cur.BaseURL = kiroBaseURLOr(proto, r.FormValue("base_url"))
	cur.Protocol = proto
	cur.AuthMethod = domain.AuthOAuth
	cur.AuthScheme = domain.AuthBearer
	cur.APIKey = ""
	// Preserve current dialect when the form omits the field (fixed backends
	// still submit a locked hidden value).
	if r.FormValue("reasoning_dialect") != "" || reasoningDialectEditable(proto) {
		cur.ReasoningDialect = dialect
	}
	if err := h.store.UpdateProvider(r.Context(), cur); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
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
		htmxBadRequest(w, r, "provider-flash", "bad id")
		return
	}
	if err := h.store.DeleteProvider(r.Context(), id); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.renderProviderList(w, r)
}

func (h *Handler) archiveProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "bad id")
		return
	}
	if err := h.store.SetProviderArchived(r.Context(), id, true); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.renderProviderList(w, r)
}

func (h *Handler) restoreProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		htmxBadRequest(w, r, "provider-flash", "bad id")
		return
	}
	if err := h.store.SetProviderArchived(r.Context(), id, false); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.renderProviderList(w, r)
}

func (h *Handler) deleteAllArchived(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteArchivedProviders(r.Context()); err != nil {
		htmxBadRequest(w, r, "provider-flash", err.Error())
		return
	}
	h.renderProviderList(w, r)
}

// refreshAllOAuth force-refreshes every saved oauth provider's access token,
// persisting the rotated tokens, and reports a one-line summary. Failures are
// collected per provider rather than aborting the batch, so one revoked refresh
// token does not block refreshing the rest. Non-refreshable device-token
// providers (e.g. Qoder) are skipped, not failed: their tokens cannot rotate,
// so attempting a refresh would always surface a false "reconnect required".
// Liveness for those is the Check button / live upstream call, not bulk refresh.
func (h *Handler) refreshAllOAuth(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		render(w, r, CheckResult(false, err.Error()))
		return
	}
	var refreshed, failed, skipped int
	var problems []string
	for _, p := range providers {
		if p.Method() != domain.AuthOAuth || p.Archived {
			continue
		}
		if !oauth.CanRefresh(p.OAuthCreds) {
			skipped++
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
		msg := fmt.Sprintf("refreshed %d oauth provider(s)", refreshed)
		if skipped > 0 {
			msg += fmt.Sprintf(", skipped %d (non-refreshable)", skipped)
		}
		render(w, r, CheckResult(true, msg))
		return
	}
	msg := fmt.Sprintf("refreshed %d, %d failed", refreshed, failed)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d skipped", skipped)
	}
	msg += ": " + strings.Join(problems, "; ")
	render(w, r, CheckResult(false, msg))
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
	render(w, r, CombosPage(combos, activeProviders(providers)))
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
		htmxBadRequest(w, r, "combo-flash", "invalid form")
		return
	}
	c, err := parseComboForm(r)
	if err != nil {
		htmxBadRequest(w, r, "combo-flash", err.Error())
		return
	}
	if err := h.store.CreateCombo(r.Context(), c); err != nil {
		htmxBadRequest(w, r, "combo-flash", err.Error())
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
	enabledVals := r.Form["enabled"]
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
		en := true
		if i < len(enabledVals) {
			en = strings.TrimSpace(enabledVals[i]) != "0"
		}
		targets = append(targets, domain.ComboTarget{ProviderID: pid, UpstreamModel: model, Enabled: en})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("a combo needs at least one provider + model target")
	}
	hasEnabled := false
	for _, t := range targets {
		if t.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return nil, fmt.Errorf("at least one target must be enabled")
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
	render(w, r, ComboEditRow(c, comboEditProviders(activeProviders(providers), c)))
}

// comboEditProviders is the provider list offered in a combo's edit dropdowns:
// the active providers plus any archived provider a target already references,
// so editing does not silently re-point a target off its archived provider.
func comboEditProviders(active []*domain.Provider, c *domain.Combo) []*domain.Provider {
	seen := make(map[int64]bool, len(active))
	out := make([]*domain.Provider, 0, len(active)+len(c.Targets))
	for _, p := range active {
		out = append(out, p)
		seen[p.ID] = true
	}
	for _, t := range c.Targets {
		if t.Provider != nil && !seen[t.Provider.ID] {
			out = append(out, t.Provider)
			seen[t.Provider.ID] = true
		}
	}
	return out
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
		htmxBadRequest(w, r, "combo-flash", "bad id")
		return
	}
	if err := r.ParseForm(); err != nil {
		htmxBadRequest(w, r, "combo-flash", "invalid form")
		return
	}
	c, err := parseComboForm(r)
	if err != nil {
		htmxBadRequest(w, r, "combo-flash", err.Error())
		return
	}
	c.ID = id
	if err := h.store.UpdateCombo(r.Context(), c); err != nil {
		htmxBadRequest(w, r, "combo-flash", err.Error())
		return
	}
	h.renderComboList(w, r)
}

func (h *Handler) deleteCombo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		htmxBadRequest(w, r, "combo-flash", "bad id")
		return
	}
	if err := h.store.DeleteCombo(r.Context(), id); err != nil {
		htmxBadRequest(w, r, "combo-flash", err.Error())
		return
	}
	h.renderComboList(w, r)
}

func (h *Handler) toggleComboTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		htmxBadRequest(w, r, "combo-flash", "bad id")
		return
	}
	tid, err := strconv.ParseInt(r.PathValue("tid"), 10, 64)
	if err != nil {
		htmxBadRequest(w, r, "combo-flash", "bad target id")
		return
	}
	combo, err := h.store.GetCombo(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var cur *domain.ComboTarget
	for i := range combo.Targets {
		if combo.Targets[i].ID == tid {
			cur = &combo.Targets[i]
			break
		}
	}
	if cur == nil {
		http.NotFound(w, r)
		return
	}
	// Disabling is rejected when this is the last enabled target, matching
	// parseComboForm so list toggles and edit-form saves share one rule.
	if cur.Enabled {
		enabledCount := 0
		for _, t := range combo.Targets {
			if t.Enabled {
				enabledCount++
			}
		}
		if enabledCount <= 1 {
			htmxBadRequest(w, r, "combo-flash", "at least one target must be enabled")
			return
		}
	}
	if err := h.store.SetTargetEnabled(r.Context(), tid, !cur.Enabled); err != nil {
		htmxBadRequest(w, r, "combo-flash", err.Error())
		return
	}
	combo, err = h.store.GetCombo(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	render(w, r, ComboRow(combo))
}

func (h *Handler) swapComboForm(w http.ResponseWriter, r *http.Request) {
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
	combos, err := h.store.ListCombos(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	others := make([]*domain.Combo, 0, len(combos))
	for _, o := range combos {
		if o.ID != id {
			others = append(others, o)
		}
	}
	render(w, r, ComboSwapRow(c, others))
}

func (h *Handler) swapCombo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		htmxBadRequest(w, r, "combo-flash", "bad id")
		return
	}
	if err := r.ParseForm(); err != nil {
		htmxBadRequest(w, r, "combo-flash", "invalid form")
		return
	}
	other, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("other")), 10, 64)
	if err != nil || other <= 0 {
		htmxBadRequest(w, r, "combo-flash", "pick a combo to swap with")
		return
	}
	if err := h.store.SwapComboNames(r.Context(), id, other); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			htmxBadRequest(w, r, "combo-flash", "combo not found")
			return
		}
		htmxBadRequest(w, r, "combo-flash", err.Error())
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
		htmxBadRequest(w, r, "key-flash", "invalid form")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		htmxBadRequest(w, r, "key-flash", "label is required")
		return
	}
	created, err := h.store.NewAccessKey(r.Context(), name)
	if err != nil {
		htmxBadRequest(w, r, "key-flash", err.Error())
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
		htmxBadRequest(w, r, "key-flash", "bad id")
		return
	}
	if err := h.store.DeleteAccessKey(r.Context(), id); err != nil {
		htmxBadRequest(w, r, "key-flash", err.Error())
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

const logsPageSize = 20

func parseLogQuery(r *http.Request) store.RequestLogQuery {
	q := store.RequestLogQuery{
		Combo:       strings.TrimSpace(r.URL.Query().Get("combo")),
		Provider:    strings.TrimSpace(r.URL.Query().Get("provider")),
		StatusClass: strings.TrimSpace(r.URL.Query().Get("status")),
		Limit:       logsPageSize,
		Page:        1,
	}
	switch q.StatusClass {
	case "", "ok", "client", "server", "error":
	default:
		q.StatusClass = ""
	}
	if raw := r.URL.Query().Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			q.Page = n
		}
	}
	return q
}

func (h *Handler) logFilterOpts(ctx context.Context) (LogFilterOpts, error) {
	combos, err := h.store.DistinctRequestLogValues(ctx, "combo")
	if err != nil {
		return LogFilterOpts{}, err
	}
	providers, err := h.store.DistinctRequestLogValues(ctx, "provider")
	if err != nil {
		return LogFilterOpts{}, err
	}
	return LogFilterOpts{Combos: combos, Providers: providers}, nil
}

func (h *Handler) logsPage(w http.ResponseWriter, r *http.Request) {
	q := parseLogQuery(r)
	total, err := h.store.CountRequestLogsQuery(r.Context(), q)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	meta := logsPageMeta(q.Page, logsPageSize, total)
	// Clamp page if the URL is past the last page after deletes/filters.
	q.Page = meta.Page
	q.Limit = logsPageSize
	logs, err := h.store.ListRequestLogsQuery(r.Context(), q)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	reqs, in, out, err := h.store.RequestLogStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	opts, err := h.logFilterOpts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	stats := LogStats{TotalReqs: reqs, TotalIn: in, TotalOut: out}
	if r.Header.Get("HX-Request") != "" {
		render(w, r, LogsBody(logs, stats, q, opts, meta))
		return
	}
	render(w, r, LogsPage(logs, stats, q, opts, meta))
}

func (h *Handler) clearLogs(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ClearRequestLogs(r.Context()); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	q := store.RequestLogQuery{Limit: logsPageSize, Page: 1}
	render(w, r, LogsBody(nil, LogStats{}, q, LogFilterOpts{}, logsPageMeta(1, logsPageSize, 0)))
}

// --- Settings / import-export ---

// HARView is a safe DTO for the settings HAR panel (no filesystem paths).
type HARView struct {
	Mode         string
	State        string
	StartedAt    string
	StoppedAt    string
	Pages        int
	Entries      int
	InFlight     int
	CanStart     bool
	CanStop      bool
	CanDownload  bool
	Poll         bool
	FileMode     bool
	StateLabel   string
	Detail       string
	ErrorMessage string
}

func (h *Handler) harView(errMsg string) HARView {
	st := harlog.Status{Mode: harlog.ModeRuntime, State: harlog.StateIdle}
	if h.har != nil {
		st = h.har.Status()
	}
	v := HARView{
		Mode:         st.Mode.String(),
		State:        st.State.String(),
		Pages:        st.Pages,
		Entries:      st.Entries,
		InFlight:     st.InFlight,
		FileMode:     st.Mode == harlog.ModeFile,
		ErrorMessage: errMsg,
	}
	if !st.StartedAt.IsZero() {
		v.StartedAt = st.StartedAt.Local().Format(time.RFC3339)
	}
	if !st.StoppedAt.IsZero() {
		v.StoppedAt = st.StoppedAt.Local().Format(time.RFC3339)
	}
	switch {
	case v.FileMode:
		v.StateLabel = "Always recording"
		v.CanDownload = true
		v.Detail = "Capture is always on because -har-file is set. Download is a live snapshot. The configured path is flushed on shutdown."
	case st.State == harlog.StateIdle:
		v.StateLabel = "Idle"
		v.CanStart = true
		v.Detail = "No active recording. Start to capture proxied requests until you stop."
	case st.State == harlog.StateRecording:
		v.StateLabel = "Recording"
		v.CanStop = true
		v.Poll = true
		v.Detail = "New proxied requests are captured. Download is available only after Stop finishes."
	case st.State == harlog.StateStopping:
		v.StateLabel = "Stopping"
		v.Poll = true
		v.Detail = "New requests are excluded. Waiting for in-flight requests to finish before download is available."
	case st.State == harlog.StateStopped:
		v.StateLabel = "Stopped"
		v.CanStart = true
		v.CanDownload = true
		v.Detail = "Recording retained in memory. Download the HAR, or start a new recording to replace it."
	}
	return v
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, SettingsPage(h.harView("")))
}

func (h *Handler) harStatus(w http.ResponseWriter, r *http.Request) {
	render(w, r, HARPanel(h.harView("")))
}

func (h *Handler) harStart(w http.ResponseWriter, r *http.Request) {
	if h.har == nil {
		htmxBadRequest(w, r, "har-flash", "HAR capture is unavailable")
		return
	}
	if err := h.har.Start(); err != nil {
		msg := harControlError(err)
		htmxBadRequest(w, r, "har-flash", msg)
		return
	}
	render(w, r, HARPanel(h.harView("")))
}

func (h *Handler) harStop(w http.ResponseWriter, r *http.Request) {
	if h.har == nil {
		htmxBadRequest(w, r, "har-flash", "HAR capture is unavailable")
		return
	}
	if err := h.har.Stop(); err != nil {
		msg := harControlError(err)
		htmxBadRequest(w, r, "har-flash", msg)
		return
	}
	render(w, r, HARPanel(h.harView("")))
}

func harControlError(err error) string {
	switch {
	case errors.Is(err, harlog.ErrFileMode):
		return "Start/Stop is disabled while -har-file is set"
	case errors.Is(err, harlog.ErrInvalidTransition):
		return "HAR control is not valid in the current state"
	default:
		return "HAR control failed"
	}
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
	sum, err := h.store.Import(r.Context(), file)
	if err != nil {
		render(w, r, flash("error", "import failed: "+err.Error()))
		return
	}
	msg := fmt.Sprintf("Import complete: %d providers (%d new, %d updated), %d combos (%d new, %d updated) — review Providers and Combos",
		sum.ProvidersCreated+sum.ProvidersUpdated, sum.ProvidersCreated, sum.ProvidersUpdated,
		sum.CombosCreated+sum.CombosUpdated, sum.CombosCreated, sum.CombosUpdated)
	if len(sum.Failures) > 0 {
		msg += fmt.Sprintf("; %d skipped: %s", len(sum.Failures), strings.Join(sum.Failures, "; "))
		render(w, r, flash("warn", msg))
		return
	}
	render(w, r, flash("ok", msg))
}
