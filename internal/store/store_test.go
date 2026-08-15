package store

import (
	"context"
	"testing"

	"airouter/internal/domain"
)

// TestMaxOpenConnsCapped locks the invariant that Open caps the SQLite pool.
// SQLite serializes writes through a single write lock; an unbounded pool would
// multiply lock contenders and file handles under dashboard + proxy load.
func TestMaxOpenConnsCapped(t *testing.T) {
	st := testStore(t)
	stats := st.db.Stats()
	if stats.MaxOpenConnections != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", stats.MaxOpenConnections)
	}
}

// TestMigrateProviderAntigravityBaseURL rewrites only Antigravity rows still on
// the old prod cloudcode-pa default. Custom URLs and other protocols stay put.
func TestMigrateProviderAntigravityBaseURL(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	const (
		oldDefault = "https://cloudcode-pa.googleapis.com"
		newDefault = "https://daily-cloudcode-pa.googleapis.com"
		customURL  = "https://example.invalid/ag"
	)

	old := &domain.Provider{
		Name: "ag-old", BaseURL: oldDefault, APIKey: "k",
		Protocol: domain.ProtocolAntigravity,
	}
	custom := &domain.Provider{
		Name: "ag-custom", BaseURL: customURL, APIKey: "k",
		Protocol: domain.ProtocolAntigravity,
	}
	other := &domain.Provider{
		Name: "not-ag", BaseURL: oldDefault, APIKey: "k",
		Protocol: domain.ProtocolOpenAI,
	}
	for _, p := range []*domain.Provider{old, custom, other} {
		if err := st.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.migrateProviderAntigravityBaseURL(); err != nil {
		t.Fatal(err)
	}

	gotOld, err := st.GetProvider(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.BaseURL != newDefault {
		t.Errorf("old default = %q, want %q", gotOld.BaseURL, newDefault)
	}

	gotCustom, err := st.GetProvider(ctx, custom.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotCustom.BaseURL != customURL {
		t.Errorf("custom = %q, want unchanged %q", gotCustom.BaseURL, customURL)
	}

	gotOther, err := st.GetProvider(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOther.BaseURL != oldDefault {
		t.Errorf("non-antigravity = %q, want unchanged %q", gotOther.BaseURL, oldDefault)
	}

	// Second run is a no-op: already-migrated and untouched rows stay put.
	if err := st.migrateProviderAntigravityBaseURL(); err != nil {
		t.Fatal(err)
	}
	gotOld, err = st.GetProvider(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.BaseURL != newDefault {
		t.Errorf("second run = %q, want %q", gotOld.BaseURL, newDefault)
	}
}
