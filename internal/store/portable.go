package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"airouter/internal/domain"
)

// Portable config formats. API keys are exported in PLAINTEXT so the config can
// be moved between instances with different encryption secrets. Treat exported
// files as secrets. Access keys cannot be exported (only their hashes exist),
// so they are intentionally omitted.

type portableProvider struct {
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	Protocol string `json:"protocol"`
}

type portableCombo struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"` // provider name, not id, for portability
	UpstreamModel string `json:"upstream_model"`
}

type portableConfig struct {
	Version   int                `json:"version"`
	Providers []portableProvider `json:"providers"`
	Combos    []portableCombo    `json:"combos"`
}

func (s *Store) Export(ctx context.Context, w io.Writer) error {
	providers, err := s.ListProviders(ctx)
	if err != nil {
		return err
	}
	combos, err := s.ListCombos(ctx)
	if err != nil {
		return err
	}
	cfg := portableConfig{Version: 1}
	for _, p := range providers {
		cfg.Providers = append(cfg.Providers, portableProvider{
			Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Protocol: string(p.Protocol),
		})
	}
	for _, c := range combos {
		cfg.Combos = append(cfg.Combos, portableCombo{
			Name: c.Name, Provider: c.Provider.Name, UpstreamModel: c.UpstreamModel,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

// Import upserts providers and combos by name. Existing entries with the same
// name are updated; others are created. It does not delete anything.
func (s *Store) Import(ctx context.Context, r io.Reader) error {
	var cfg portableConfig
	if err := json.NewDecoder(r).Decode(&cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}

	existingProviders, err := s.ListProviders(ctx)
	if err != nil {
		return err
	}
	byName := map[string]*domain.Provider{}
	for _, p := range existingProviders {
		byName[p.Name] = p
	}

	for _, pp := range cfg.Providers {
		proto := domain.Protocol(pp.Protocol)
		if !proto.Valid() {
			return fmt.Errorf("provider %q: invalid protocol %q", pp.Name, pp.Protocol)
		}
		if cur, ok := byName[pp.Name]; ok {
			cur.BaseURL, cur.APIKey, cur.Protocol = pp.BaseURL, pp.APIKey, proto
			if err := s.UpdateProvider(ctx, cur); err != nil {
				return err
			}
		} else {
			np := &domain.Provider{Name: pp.Name, BaseURL: pp.BaseURL, APIKey: pp.APIKey, Protocol: proto}
			if err := s.CreateProvider(ctx, np); err != nil {
				return err
			}
			byName[np.Name] = np
		}
	}

	existingCombos, err := s.ListCombos(ctx)
	if err != nil {
		return err
	}
	comboByName := map[string]*domain.Combo{}
	for _, c := range existingCombos {
		comboByName[c.Name] = c
	}

	for _, pc := range cfg.Combos {
		prov, ok := byName[pc.Provider]
		if !ok {
			return fmt.Errorf("combo %q references unknown provider %q", pc.Name, pc.Provider)
		}
		if cur, ok := comboByName[pc.Name]; ok {
			cur.ProviderID, cur.UpstreamModel = prov.ID, pc.UpstreamModel
			if err := s.UpdateCombo(ctx, cur); err != nil {
				return err
			}
		} else {
			if err := s.CreateCombo(ctx, &domain.Combo{
				Name: pc.Name, ProviderID: prov.ID, UpstreamModel: pc.UpstreamModel,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
