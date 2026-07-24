package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"airouter/internal/crypto"
	"airouter/internal/domain"
)

// openRawLegacy opens a database and creates the pre-multi-target schema
// (combos with embedded provider_id/upstream_model, no combo_targets table),
// simulating a database written by an older binary.
func openRawLegacy(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	const legacySchema = `
CREATE TABLE providers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	base_url TEXT NOT NULL,
	api_key TEXT NOT NULL,
	protocol TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE combos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	upstream_model TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	return db
}

func testStore(t *testing.T) *Store {
	t.Helper()
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestComboTargetsRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "p1", BaseURL: "http://a", APIKey: "k1", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "p2", BaseURL: "http://b", APIKey: "k2", Protocol: domain.ProtocolAnthropic}
	for _, p := range []*domain.Provider{p1, p2} {
		if err := st.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	c := &domain.Combo{
		Name:     "default",
		Strategy: domain.StrategyRoundRobin,
		Targets: []domain.ComboTarget{
			{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
			{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
		},
	}
	if err := st.CreateCombo(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetComboByName(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != domain.StrategyRoundRobin {
		t.Errorf("strategy = %q, want roundrobin", got.Strategy)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(got.Targets))
	}
	if got.Targets[0].UpstreamModel != "m1" || got.Targets[0].Provider.Name != "p1" {
		t.Errorf("target 0 = %+v", got.Targets[0])
	}
	if got.Targets[0].Provider.APIKey != "k1" {
		t.Errorf("target 0 key = %q, want decrypted k1", got.Targets[0].Provider.APIKey)
	}
	if got.Targets[1].UpstreamModel != "m2" || got.Targets[1].Provider.Protocol != domain.ProtocolAnthropic {
		t.Errorf("target 1 = %+v", got.Targets[1])
	}
	if !got.Targets[0].Enabled || !got.Targets[1].Enabled {
		t.Errorf("targets should default to enabled: %+v %+v", got.Targets[0].Enabled, got.Targets[1].Enabled)
	}

	if err := st.SetTargetEnabled(ctx, got.Targets[0].ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetComboByName(ctx, "default")
	if got.Targets[0].Enabled {
		t.Errorf("target 0 still enabled after SetTargetEnabled(false)")
	}
	if !got.Targets[1].Enabled {
		t.Errorf("target 1 should remain enabled")
	}

	// Update replaces targets wholesale and preserves order.
	c.Targets = []domain.ComboTarget{{ProviderID: p2.ID, UpstreamModel: "only", Enabled: true}}
	c.Strategy = domain.StrategyFailover
	if err := st.UpdateCombo(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetComboByName(ctx, "default")
	if len(got.Targets) != 1 || got.Targets[0].UpstreamModel != "only" || got.Strategy != domain.StrategyFailover {
		t.Errorf("after update: %+v", got)
	}
}

// TestLegacyComboMigration creates a database with the pre-multi-target schema,
// then reopens it through the current store to verify each legacy combo becomes
// a single position-0 target and the combos table is rebuilt.
func TestLegacyComboMigration(t *testing.T) {
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build the legacy schema by hand and seed one provider + combo.
	legacy := openRawLegacy(t, path)
	enc, err := c.Encrypt("k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(
		"INSERT INTO providers (name, base_url, api_key, protocol) VALUES ('p1','http://a',?,'openai')", enc); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(
		"INSERT INTO combos (name, provider_id, upstream_model) VALUES ('legacy', 1, 'gpt')"); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	// Reopen through the migrating store.
	migrated, err := Open(path, c)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	got, err := migrated.GetComboByName(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != domain.StrategyFailover {
		t.Errorf("strategy = %q, want failover", got.Strategy)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(got.Targets))
	}
	if got.Targets[0].UpstreamModel != "gpt" || got.Targets[0].Provider.Name != "p1" {
		t.Errorf("migrated target = %+v", got.Targets[0])
	}
	if got.Targets[0].Provider.APIKey != "k1" {
		t.Errorf("migrated key = %q, want k1", got.Targets[0].Provider.APIKey)
	}
	if !got.Targets[0].Enabled {
		t.Errorf("migrated target should default to enabled")
	}

	// Reopening again must be a no-op (idempotent migration).
	again, err := Open(path, c)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	again.Close()
}

func TestSwapComboNames(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "p1", BaseURL: "http://a", APIKey: "k1", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "p2", BaseURL: "http://b", APIKey: "k2", Protocol: domain.ProtocolAnthropic}
	for _, p := range []*domain.Provider{p1, p2} {
		if err := st.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	alpha := &domain.Combo{
		Name:     "alpha",
		Strategy: domain.StrategyFailover,
		Targets:  []domain.ComboTarget{{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true}},
	}
	beta := &domain.Combo{
		Name:     "beta",
		Strategy: domain.StrategyRoundRobin,
		Targets:  []domain.ComboTarget{{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true}},
	}
	for _, c := range []*domain.Combo{alpha, beta} {
		if err := st.CreateCombo(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.SwapComboNames(ctx, alpha.ID, beta.ID); err != nil {
		t.Fatalf("SwapComboNames: %v", err)
	}

	gotAlpha, err := st.GetCombo(ctx, alpha.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotBeta, err := st.GetCombo(ctx, beta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAlpha.Name != "beta" {
		t.Errorf("alpha name = %q, want beta", gotAlpha.Name)
	}
	if gotBeta.Name != "alpha" {
		t.Errorf("beta name = %q, want alpha", gotBeta.Name)
	}
	// ids, targets, and strategy stay with the original combo identities.
	if gotAlpha.Strategy != domain.StrategyFailover || len(gotAlpha.Targets) != 1 ||
		gotAlpha.Targets[0].UpstreamModel != "m1" || gotAlpha.Targets[0].Provider.Name != "p1" {
		t.Errorf("alpha identity changed: %+v", gotAlpha)
	}
	if gotBeta.Strategy != domain.StrategyRoundRobin || len(gotBeta.Targets) != 1 ||
		gotBeta.Targets[0].UpstreamModel != "m2" || gotBeta.Targets[0].Provider.Name != "p2" {
		t.Errorf("beta identity changed: %+v", gotBeta)
	}

	// Hot path resolves the swapped names to the exchanged ids.
	byName, err := st.GetComboByName(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != beta.ID {
		t.Errorf("GetComboByName(alpha).ID = %d, want %d", byName.ID, beta.ID)
	}
	byName, err = st.GetComboByName(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != alpha.ID {
		t.Errorf("GetComboByName(beta).ID = %d, want %d", byName.ID, alpha.ID)
	}

	// Swapping back restores the original names.
	if err := st.SwapComboNames(ctx, alpha.ID, beta.ID); err != nil {
		t.Fatalf("swap back: %v", err)
	}
	gotAlpha, _ = st.GetCombo(ctx, alpha.ID)
	gotBeta, _ = st.GetCombo(ctx, beta.ID)
	if gotAlpha.Name != "alpha" || gotBeta.Name != "beta" {
		t.Errorf("after swap-back: alpha=%q beta=%q", gotAlpha.Name, gotBeta.Name)
	}

	// Self-swap is rejected.
	if err := st.SwapComboNames(ctx, alpha.ID, alpha.ID); err == nil {
		t.Errorf("swapping a combo with itself should error")
	}
	// Missing id surfaces ErrNotFound.
	if err := st.SwapComboNames(ctx, alpha.ID, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteComboRemovesRowAndCascadesTargets(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := &domain.Provider{Name: "p", BaseURL: "http://a", APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	c := &domain.Combo{
		Name:     "del",
		Strategy: domain.StrategyFailover,
		Targets: []domain.ComboTarget{
			{ProviderID: p.ID, UpstreamModel: "m1", Enabled: true},
			{ProviderID: p.ID, UpstreamModel: "m2", Enabled: true},
		},
	}
	if err := st.CreateCombo(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Confirm targets were inserted.
	var n int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM combo_targets WHERE combo_id = ?", c.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("target rows before delete = %d, want 2", n)
	}

	if err := st.DeleteCombo(ctx, c.ID); err != nil {
		t.Fatalf("DeleteCombo: %v", err)
	}

	// Combo row gone.
	if _, err := st.GetCombo(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetCombo after delete err = %v, want ErrNotFound", err)
	}
	// Targets cascade-deleted (FK ON DELETE CASCADE).
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM combo_targets WHERE combo_id = ?", c.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("target rows after delete = %d, want 0 (cascade)", n)
	}
}
