package cursor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
)

// ListModelIDs returns the account's usable model ids, falling back to
// StaticModels on any failure (so combo autocomplete still works offline).
// p.APIKey must hold the resolved access token (the caller resolves it for oauth
// providers); the machine id comes from OAuthCreds.
func ListModelIDs(ctx context.Context, p *domain.Provider) []string {
	token, machineID := providerCreds(p)
	if token == "" {
		return append([]string(nil), StaticModels...)
	}
	ids, err := fetchUsableModels(ctx, token, machineID)
	if err != nil || len(ids) == 0 {
		return append([]string(nil), StaticModels...)
	}
	return ids
}

// CheckModels probes GetUsableModels to validate the token without sending a
// chat. Returns ok and a human message. Used by the dashboard Check button.
func CheckModels(ctx context.Context, p *domain.Provider) (bool, string) {
	token, machineID := providerCreds(p)
	if token == "" {
		return false, "no access token - paste one or reconnect"
	}
	url := AgentBaseURL + ModelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return false, "invalid request"
	}
	h := BuildModelsHeaders(token, machineID, true)
	for k, vv := range h {
		for _, v := range vv {
			req.Header.Set(k, v)
		}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, "could not reach Cursor: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("token rejected (HTTP %d) - reconnect required", resp.StatusCode)
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	ids := ParseUsableModels(body)
	return true, fmt.Sprintf("OK - reachable, token accepted (%d models)", len(ids))
}

// providerCreds extracts the access token and machine id from a provider. The
// token is stripped of any "::" prefix; the machine id falls back to a
// token-derived hash so headers are well-formed even without a paste.
func providerCreds(p *domain.Provider) (token, machineID string) {
	if p == nil {
		return "", ""
	}
	token = stripColonPrefix(strings.TrimSpace(p.APIKey))
	if p.OAuthCreds != nil {
		machineID = strings.TrimSpace(p.OAuthCreds.MachineID)
	}
	if machineID == "" && token != "" {
		machineID = machineIDFallback(token)
	}
	return token, machineID
}

// fetchUsableModels calls the AgentService GetUsableModels unary RPC. It uses an
// unframed application/proto body (unlike chat's connect+proto frames). Go's
// net/http negotiates HTTP/2 over TLS automatically via ALPN, matching the
// h2-only endpoint without a dedicated h2 client.
func fetchUsableModels(ctx context.Context, token, machineID string) ([]string, error) {
	url := AgentBaseURL + ModelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return nil, err
	}
	h := BuildModelsHeaders(token, machineID, true)
	for k, vv := range h {
		for _, v := range vv {
			req.Header.Set(k, v)
		}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cursor: GetUsableModels HTTP %d", resp.StatusCode)
	}
	return ParseUsableModels(body), nil
}

// ParseUsableModels decodes agent.v1.GetUsableModelsResponse: repeated field 1
// = ModelDetails { id(1), display_model_id(3), display_name(4),
// display_name_short(5) }. Prefers id(1); falls back to display_model_id(3).
func ParseUsableModels(payload []byte) []string {
	m, err := decodeMessage(payload)
	if err != nil {
		return nil
	}
	entries, ok := m[1] // RESPONSE_MODELS_FIELD
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		detail, err := decodeMessage(e.value)
		if err != nil {
			continue
		}
		id, _ := stringField(detail, 1) // MODEL_ID_FIELD
		id = strings.TrimSpace(id)
		if id == "" {
			id, _ = stringField(detail, 3) // DISPLAY_MODEL_ID_FIELD
			id = strings.TrimSpace(id)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
