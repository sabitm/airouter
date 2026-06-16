package store

import (
	"context"
	"database/sql"
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
			{ProviderID: p1.ID, UpstreamModel: "m1"},
			{ProviderID: p2.ID, UpstreamModel: "m2"},
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

	// Update replaces targets wholesale and preserves order.
	c.Targets = []domain.ComboTarget{{ProviderID: p2.ID, UpstreamModel: "only"}}
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

	// Reopening again must be a no-op (idempotent migration).
	again, err := Open(path, c)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	again.Close()
}
