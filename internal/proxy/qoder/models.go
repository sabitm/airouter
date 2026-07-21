package qoder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"airouter/internal/domain"
)

const catalogTTL = time.Hour

type catalogEntry struct {
	expiresAt  time.Time
	models     []string
	rawConfigs map[string]json.RawMessage
}

var (
	catalogMu sync.Mutex
	catalog   = map[string]*catalogEntry{}
	inflight  = map[string]*catalogWait{}
)

type catalogWait struct {
	done chan struct{}
	err  error
	ent  *catalogEntry
}

// LookupModelConfig returns the server-published model_config for key.
// On cache miss it fetches the live catalog once (and once more if forced).
func LookupModelConfig(ctx context.Context, provider *domain.Provider, key string) (json.RawMessage, error) {
	ent, err := resolveCatalog(ctx, provider, false)
	if err != nil {
		return nil, err
	}
	if raw, ok := ent.rawConfigs[key]; ok {
		return append(json.RawMessage(nil), raw...), nil
	}
	// Force refresh once for first-ever or newly published models.
	ent, err = resolveCatalog(ctx, provider, true)
	if err != nil {
		return nil, err
	}
	if raw, ok := ent.rawConfigs[key]; ok {
		return append(json.RawMessage(nil), raw...), nil
	}
	return nil, fmt.Errorf("qoder: model_config for %q not known (fetch model list or check connectivity)", key)
}

// ListModelIDs returns enabled model keys for dashboard autocomplete.
func ListModelIDs(ctx context.Context, provider *domain.Provider) ([]string, error) {
	ent, err := resolveCatalog(ctx, provider, false)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), ent.models...)
	return out, nil
}

func resolveCatalog(ctx context.Context, provider *domain.Provider, force bool) (*catalogEntry, error) {
	ck := cacheKey(provider)
	now := time.Now()
	if !force {
		catalogMu.Lock()
		if ent, ok := catalog[ck]; ok && ent.expiresAt.After(now) {
			catalogMu.Unlock()
			return ent, nil
		}
		if w, ok := inflight[ck]; ok {
			catalogMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-w.done:
				if w.err != nil {
					return nil, w.err
				}
				return w.ent, nil
			}
		}
		w := &catalogWait{done: make(chan struct{})}
		inflight[ck] = w
		catalogMu.Unlock()

		ent, err := fetchCatalog(ctx, provider)
		catalogMu.Lock()
		delete(inflight, ck)
		if err != nil {
			w.err = err
			close(w.done)
			catalogMu.Unlock()
			return nil, err
		}
		catalog[ck] = ent
		w.ent = ent
		close(w.done)
		catalogMu.Unlock()
		return ent, nil
	}

	// force: always fetch; coalesce only non-force callers.
	ent, err := fetchCatalog(ctx, provider)
	if err != nil {
		return nil, err
	}
	catalogMu.Lock()
	catalog[ck] = ent
	catalogMu.Unlock()
	return ent, nil
}

func cacheKey(provider *domain.Provider) string {
	creds := CredsFromProvider(provider)
	seed := creds.UserID
	if seed == "" {
		seed = creds.AuthToken
	}
	if seed == "" {
		seed = "anonymous"
	}
	sum := sha256.Sum256([]byte("qoder:" + seed))
	return hex.EncodeToString(sum[:])
}

func fetchCatalog(ctx context.Context, provider *domain.Provider) (*catalogEntry, error) {
	creds := CredsFromProvider(provider)
	if creds.UserID == "" || creds.AuthToken == "" {
		return nil, fmt.Errorf("qoder: missing user id or access token for model list")
	}
	headers, err := BuildCosyHeaders([]byte{}, ModelListURL, creds)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelListURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qoder: model list HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Chat []json.RawMessage `json:"chat"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("qoder: model list parse: %w", err)
	}
	ent := &catalogEntry{
		expiresAt:  time.Now().Add(catalogTTL),
		rawConfigs: map[string]json.RawMessage{},
	}
	for _, raw := range parsed.Chat {
		var entry struct {
			Key     string `json:"key"`
			Enable  *bool  `json:"enable"`
			Display string `json:"display_name"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Key == "" {
			continue
		}
		// Always cache config (chat needs disabled keys too).
		ent.rawConfigs[entry.Key] = append(json.RawMessage(nil), raw...)
		if entry.Enable != nil && !*entry.Enable {
			continue
		}
		ent.models = append(ent.models, entry.Key)
	}
	return ent, nil
}

// SeedCatalogForTest installs a catalog entry for tests.
func SeedCatalogForTest(provider *domain.Provider, configs map[string]json.RawMessage, models []string) {
	ck := cacheKey(provider)
	catalogMu.Lock()
	catalog[ck] = &catalogEntry{
		expiresAt:  time.Now().Add(catalogTTL),
		models:     append([]string(nil), models...),
		rawConfigs: configs,
	}
	catalogMu.Unlock()
}

// ClearCatalogForTest wipes the in-memory catalog.
func ClearCatalogForTest() {
	catalogMu.Lock()
	catalog = map[string]*catalogEntry{}
	catalogMu.Unlock()
}
