package proxy

import (
	"encoding/json"
	"net/http"
)

// handleModels returns the configured combos as an OpenAI-style model list.
// Combo names are what clients put in the request `model` field, so this is the
// set of models the router actually exposes. Access-key protected.
func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	if !p.authenticate(r) {
		writeErr(w, openaiCodec, http.StatusUnauthorized, "invalid or missing access key", "authentication_error")
		return
	}
	combos, err := p.store.ListCombos(r.Context())
	if err != nil {
		writeErr(w, openaiCodec, http.StatusInternalServerError, "failed to list models", "api_error")
		return
	}
	data := make([]map[string]any, 0, len(combos))
	for _, c := range combos {
		data = append(data, map[string]any{
			"id":       c.Name,
			"object":   "model",
			"created":  c.CreatedAt.Unix(),
			"owned_by": "airouter",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
