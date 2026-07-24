package store

import (
	"context"
	"errors"
	"testing"

	"airouter/internal/domain"
)

// instrumentedCount wraps the real DB count and records how many times the DB
// was actually consulted (cache miss), so tests can assert the cache short-
// circuits subsequent calls.
func instrumentedCount(st *Store) (func(context.Context) (int, error), *int) {
	hits := new(int)
	real := st.countAccessKeysDB
	return func(ctx context.Context) (int, error) {
		*hits++
		return real(ctx)
	}, hits
}

// TestCountAccessKeysCachesAfterMiss verifies the open-mode check is cached: the
// first CountAccessKeys hits the DB; a second call (same process, no mutation)
// must not.
func TestCountAccessKeysCachesAfterMiss(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	fn, hits := instrumentedCount(st)
	st.countKeysFn = fn
	t.Cleanup(func() { st.countKeysFn = nil })

	if n, err := st.CountAccessKeys(ctx); err != nil || n != 0 {
		t.Fatalf("first Count = %d, err %v; want 0, nil", n, err)
	}
	if *hits != 1 {
		t.Fatalf("DB hits after first call = %d, want 1", *hits)
	}

	// Second call: cache populated (open mode, *p == false). Must not hit DB.
	if n, err := st.CountAccessKeys(ctx); err != nil || n != 0 {
		t.Fatalf("second Count = %d, err %v; want 0, nil", n, err)
	}
	if *hits != 1 {
		t.Fatalf("DB hits after second call = %d, want 1 (cached)", *hits)
	}
}

// TestCountAccessKeysInvalidatedByCreateDelete verifies the cache is cleared
// when the key set changes, so the open-mode gate stays correct.
func TestCountAccessKeysInvalidatedByCreateDelete(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if n, _ := st.CountAccessKeys(ctx); n != 0 {
		t.Fatalf("initial count = %d, want 0", n)
	}
	// Cache now says "no keys" (open mode). Creating one must invalidate.
	if _, err := st.NewAccessKey(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountAccessKeys(ctx); n != 1 {
		t.Fatalf("after create, count = %d, want 1", n)
	}
	// Cache now says "keys exist". Deleting the last one must invalidate back to open.
	if err := st.DeleteAccessKey(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountAccessKeys(ctx); n != 0 {
		t.Fatalf("after delete, count = %d, want 0", n)
	}
}

// TestCountAccessKeysCachedPresentShortCircuits verifies the "keys exist" branch
// returns without recomputing, so the exact count is not refetched on every call.
func TestCountAccessKeysCachedPresentShortCircuits(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if _, err := st.NewAccessKey(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	fn, hits := instrumentedCount(st)
	st.countKeysFn = fn
	t.Cleanup(func() { st.countKeysFn = nil })

	// First call after create: cache invalidated by NewAccessKey, so this hits DB.
	if n, _ := st.CountAccessKeys(ctx); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	if *hits != 1 {
		t.Fatalf("DB hits = %d, want 1", *hits)
	}
	// Subsequent calls: cached as "present", must not hit DB.
	for i := 0; i < 5; i++ {
		if _, err := st.CountAccessKeys(ctx); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if *hits != 1 {
		t.Fatalf("DB hits after cached calls = %d, want 1", *hits)
	}
}

func TestListAccessKeysEmpty(t *testing.T) {
	st := testStore(t)
	got, err := st.ListAccessKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty store returned %v, want nil", got)
	}
}

func TestListAccessKeysOrderedAndVerifyToken(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	k1, err := st.NewAccessKey(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := st.NewAccessKey(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.ListAccessKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Both keys present (created_at ties at second granularity make strict
	// order nondeterministic; assert the set, not the sequence).
	names := map[string]bool{got[0].Name: true, got[1].Name: true}
	if !names["first"] || !names["second"] {
		t.Errorf("names = %v, want {first, second}", names)
	}
	for _, k := range got {
		if k.Prefix == "" || k.Hash == "" {
			t.Errorf("key %+v missing prefix/hash", k)
		}
	}

	// VerifyToken round-trips each raw token back to the stored key.
	for _, raw := range []*domain.AccessKey{k1, k2} {
		v, err := st.VerifyToken(ctx, raw.Token)
		if err != nil {
			t.Fatalf("VerifyToken(%q): %v", raw.Token, err)
		}
		if v.ID != raw.ID || v.Name != raw.Name || v.Hash != raw.Hash {
			t.Errorf("VerifyToken got %+v, want %+v", v, raw)
		}
	}

	// Unknown token surfaces ErrNotFound (the open-mode vs reject gate).
	if _, err := st.VerifyToken(ctx, "sk-air-deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token err = %v, want ErrNotFound", err)
	}
}
