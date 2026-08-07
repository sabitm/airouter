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
	// AuthScheme is the effective credential header. Export always writes the
	// resolved value (never empty); import also accepts "" or "default" as an
	// alias for the protocol's scheme (see Provider.Auth).
	AuthScheme string `json:"auth_scheme"`
	// AuthMethod selects apikey vs oauth. Export always writes the resolved value;
	// import also accepts "" or "default" as an alias for apikey (see
	// Provider.Method).
	AuthMethod string `json:"auth_method"`
	// ReasoningDialect is optional. Export writes the effective canonical dialect.
	// Omitted/empty on import keeps the protocol default; aliases are accepted.
	ReasoningDialect string             `json:"reasoning_dialect,omitempty"`
	OAuth            *domain.OAuthCreds `json:"oauth,omitempty"`
	Archived         bool               `json:"archived,omitempty"`
}

type portableTarget struct {
	Provider      string `json:"provider"` // provider name, not id, for portability
	UpstreamModel string `json:"upstream_model"`
	Disabled      bool   `json:"disabled,omitempty"`
}

type portableCombo struct {
	Name     string           `json:"name"`
	Strategy string           `json:"strategy,omitempty"`
	Targets  []portableTarget `json:"targets,omitempty"`

	// Legacy single-target fields, accepted on import for backward compatibility.
	Provider      string `json:"provider,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
}

// resolveTargets returns the combo's targets, folding the legacy single-provider
// fields into one target when the new targets array is absent.
func (pc portableCombo) resolveTargets() []portableTarget {
	if len(pc.Targets) > 0 {
		return pc.Targets
	}
	if pc.Provider != "" {
		return []portableTarget{{Provider: pc.Provider, UpstreamModel: pc.UpstreamModel}}
	}
	return nil
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
		pp := portableProvider{
			Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Protocol: string(p.Protocol),
			AuthScheme: string(p.Auth()), AuthMethod: string(p.Method()),
			ReasoningDialect: string(p.Reasoning()), Archived: p.Archived,
		}
		if p.Method() == domain.AuthOAuth {
			pp.OAuth = p.OAuthCreds
		}
		cfg.Providers = append(cfg.Providers, pp)
	}
	for _, c := range combos {
		pc := portableCombo{Name: c.Name, Strategy: string(c.Strategy)}
		for _, t := range c.Targets {
			pc.Targets = append(pc.Targets, portableTarget{
				Provider: t.Provider.Name, UpstreamModel: t.UpstreamModel, Disabled: !t.Enabled,
			})
		}
		cfg.Combos = append(cfg.Combos, pc)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

// ImportSummary reports the outcome of an Import: per-entity counts plus one
// entry per skipped/failed row. A single bad row no longer aborts the import;
// only read/decode/transactions errors return a non-nil error.
type ImportSummary struct {
	ProvidersCreated int
	ProvidersUpdated int
	CombosCreated    int
	CombosUpdated    int
	// Failures holds human-readable "name: reason" entries for skipped rows.
	Failures []string
}

// Total returns the number of entities successfully imported.
func (s *ImportSummary) Total() int {
	return s.ProvidersCreated + s.ProvidersUpdated + s.CombosCreated + s.CombosUpdated
}

// Import upserts providers and combos by name. Existing entries with the same
// name are updated; others are created. It does not delete anything.
//
// The whole import runs in a single transaction: either every importable row
// commits together or (on a transaction/read/decode error) nothing does, so a
// failure can no longer leave providers imported but combos missing. Bad rows
// (invalid protocol, unknown provider reference, ...) are skipped and recorded
// in the returned summary rather than aborting the rest.
func (s *Store) Import(ctx context.Context, r io.Reader) (*ImportSummary, error) {
	var cfg portableConfig
	if err := json.NewDecoder(r).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sum := &ImportSummary{}

	existingProviders, err := s.listProviders(ctx, tx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*domain.Provider{}
	for _, p := range existingProviders {
		byName[p.Name] = p
	}

	for _, pp := range cfg.Providers {
		if fail := s.importProvider(ctx, tx, pp, byName, sum); fail != "" {
			sum.Failures = append(sum.Failures, fail)
		}
	}

	existingCombos, err := s.listCombos(ctx, tx)
	if err != nil {
		return nil, err
	}
	comboByName := map[string]*domain.Combo{}
	for _, c := range existingCombos {
		comboByName[c.Name] = c
	}

	for _, pc := range cfg.Combos {
		if fail := s.importCombo(ctx, tx, pc, byName, comboByName, sum); fail != "" {
			sum.Failures = append(sum.Failures, fail)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit import: %w", err)
	}
	return sum, nil
}

// importProvider validates and upserts one provider inside the import tx.
// Returns a non-empty failure string (recorded, not fatal) when the row is
// rejected; on success it bumps the summary counter.
func (s *Store) importProvider(ctx context.Context, ex executor, pp portableProvider, byName map[string]*domain.Provider, sum *ImportSummary) string {
	proto := domain.Protocol(pp.Protocol)
	if !proto.Valid() {
		return fmt.Sprintf("provider %q: invalid protocol %q", pp.Name, pp.Protocol)
	}
	// "", "default" mean the protocol's sensible scheme; any other non-scheme
	// value is rejected. The alias is expanded to a concrete value below.
	auth := domain.AuthScheme(pp.AuthScheme)
	if auth == "default" {
		auth = ""
	}
	if auth != "" && !auth.Valid() {
		return fmt.Sprintf("provider %q: invalid auth_scheme %q", pp.Name, pp.AuthScheme)
	}
	// "", "default" mean apikey; only apikey/oauth are accepted.
	method := domain.AuthMethod(pp.AuthMethod)
	if method == "default" {
		method = ""
	}
	if method != "" && !method.Valid() {
		return fmt.Sprintf("provider %q: invalid auth_method %q", pp.Name, pp.AuthMethod)
	}
	if method == domain.AuthOAuth && pp.OAuth == nil {
		return fmt.Sprintf("provider %q: auth_method oauth requires oauth credentials", pp.Name)
	}
	dialect, ok := domain.ParseReasoningDialect(pp.ReasoningDialect)
	if !ok {
		return fmt.Sprintf("provider %q: invalid reasoning_dialect %q", pp.Name, pp.ReasoningDialect)
	}
	if cur, ok := byName[pp.Name]; ok {
		cur.BaseURL, cur.APIKey, cur.Protocol = pp.BaseURL, pp.APIKey, proto
		cur.AuthScheme, cur.AuthMethod, cur.OAuthCreds, cur.Archived = auth, method, pp.OAuth, pp.Archived
		cur.ReasoningDialect = dialect
		cur.AuthScheme = cur.Auth()   // expand the default alias to a concrete scheme
		cur.AuthMethod = cur.Method() // expand the default alias to a concrete method
		if err := s.updateProvider(ctx, ex, cur); err != nil {
			return fmt.Sprintf("provider %q: %v", pp.Name, err)
		}
		sum.ProvidersUpdated++
		return ""
	}
	np := &domain.Provider{
		Name: pp.Name, BaseURL: pp.BaseURL, APIKey: pp.APIKey, Protocol: proto,
		AuthScheme: auth, AuthMethod: method, OAuthCreds: pp.OAuth, Archived: pp.Archived,
		ReasoningDialect: dialect,
	}
	np.AuthScheme = np.Auth()   // expand the default alias to a concrete scheme
	np.AuthMethod = np.Method() // expand the default alias to a concrete method
	if err := s.createProvider(ctx, ex, np); err != nil {
		return fmt.Sprintf("provider %q: %v", pp.Name, err)
	}
	byName[np.Name] = np
	sum.ProvidersCreated++
	return ""
}

// importCombo validates and upserts one combo inside the import tx; see
// importProvider for the failure-string contract.
func (s *Store) importCombo(ctx context.Context, ex executor, pc portableCombo, byName map[string]*domain.Provider, comboByName map[string]*domain.Combo, sum *ImportSummary) string {
	var targets []domain.ComboTarget
	for _, pt := range pc.resolveTargets() {
		prov, ok := byName[pt.Provider]
		if !ok {
			return fmt.Sprintf("combo %q references unknown provider %q", pc.Name, pt.Provider)
		}
		targets = append(targets, domain.ComboTarget{
			ProviderID: prov.ID, UpstreamModel: pt.UpstreamModel, Enabled: !pt.Disabled,
		})
	}
	if len(targets) == 0 {
		return fmt.Sprintf("combo %q has no targets", pc.Name)
	}
	strategy := domain.ComboStrategy(pc.Strategy)
	if strategy == "" {
		strategy = domain.StrategyFailover
	}
	if !strategy.Valid() {
		return fmt.Sprintf("combo %q: invalid strategy %q", pc.Name, pc.Strategy)
	}
	if cur, ok := comboByName[pc.Name]; ok {
		cur.Strategy, cur.Targets = strategy, targets
		if err := s.updateCombo(ctx, ex, cur); err != nil {
			return fmt.Sprintf("combo %q: %v", pc.Name, err)
		}
		sum.CombosUpdated++
		return ""
	}
	if err := s.createCombo(ctx, ex, &domain.Combo{
		Name: pc.Name, Strategy: strategy, Targets: targets,
	}); err != nil {
		return fmt.Sprintf("combo %q: %v", pc.Name, err)
	}
	sum.CombosCreated++
	return ""
}
