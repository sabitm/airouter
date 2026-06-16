package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/a-h/templ"

	"airouter/internal/domain"
	"airouter/internal/store"
)

type Handler struct {
	store *store.Store
}

func NewHandler(s *store.Store) *Handler { return &Handler{store: s} }

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
	render(w, r, ProvidersPage(providers))
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
	p := &domain.Provider{
		Name:     r.FormValue("name"),
		BaseURL:  r.FormValue("base_url"),
		APIKey:   r.FormValue("api_key"),
		Protocol: proto,
	}
	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderProviderList(w, r)
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
	cur.Name = r.FormValue("name")
	cur.BaseURL = r.FormValue("base_url")
	cur.Protocol = proto
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
	pid, err := strconv.ParseInt(r.FormValue("provider_id"), 10, 64)
	if err != nil {
		badRequest(w, "invalid provider")
		return
	}
	c := &domain.Combo{
		Name:          r.FormValue("name"),
		ProviderID:    pid,
		UpstreamModel: r.FormValue("upstream_model"),
	}
	if err := h.store.CreateCombo(r.Context(), c); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.renderComboList(w, r)
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
	pid, err := strconv.ParseInt(r.FormValue("provider_id"), 10, 64)
	if err != nil {
		badRequest(w, "invalid provider")
		return
	}
	c := &domain.Combo{
		ID:            id,
		Name:          r.FormValue("name"),
		ProviderID:    pid,
		UpstreamModel: r.FormValue("upstream_model"),
	}
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
