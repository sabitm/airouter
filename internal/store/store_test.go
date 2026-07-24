package store

import "testing"

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
