package store

import (
	"context"
	"errors"
	"testing"
	"time"

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

// TestCountAccessKeysUpdatedByCreateDelete verifies that successful mutations
// keep the cached open-mode gate consistent with the database.
func TestCountAccessKeysUpdatedByCreateDelete(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if n, _ := st.CountAccessKeys(ctx); n != 0 {
		t.Fatalf("initial count = %d, want 0", n)
	}
	// Cache now says "no keys" (open mode). Creating one must publish presence.
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

	// NewAccessKey publishes presence before it returns, so later counts do not hit the DB.
	for i := 0; i < 5; i++ {
		n, err := st.CountAccessKeys(ctx)
		if err != nil || n != 1 {
			t.Fatalf("call %d: count = %d, err %v; want 1, nil", i, n, err)
		}
	}
	if *hits != 0 {
		t.Fatalf("DB hits after cached calls = %d, want 0", *hits)
	}
}

// waitClosed fails if ch does not close before d.
func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// assertBlocked fails if ch receives before d. Use only to show an operation is
// still waiting on the cache lock, then wait for completion separately.
func assertBlocked[T any](t *testing.T, ch <-chan T, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s completed while the overlapping count still held hasKeysMu", what)
	case <-time.After(d):
	}
}

// TestCountAccessKeysCreateOverlapDoesNotPublishStaleZero forces a cache-miss
// count to observe zero, then blocks it while NewAccessKey runs. The create must
// wait for that lock, and the published cache must still report a key afterward.
func TestCountAccessKeysCreateOverlapDoesNotPublishStaleZero(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	entered := make(chan struct{})
	release := make(chan struct{})
	st.countKeysFn = func(context.Context) (int, error) {
		close(entered)
		<-release
		return 0, nil
	}
	t.Cleanup(func() { st.countKeysFn = nil })

	type countResult struct {
		n   int
		err error
	}
	countDone := make(chan countResult, 1)
	go func() {
		n, err := st.CountAccessKeys(ctx)
		countDone <- countResult{n, err}
	}()
	waitClosed(t, entered, 2*time.Second, "count to enter countKeysFn")

	createDone := make(chan error, 1)
	go func() {
		_, err := st.NewAccessKey(ctx, "k1")
		createDone <- err
	}()
	assertBlocked(t, createDone, 150*time.Millisecond, "NewAccessKey")

	close(release)
	select {
	case res := <-countDone:
		if res.err != nil || res.n != 0 {
			t.Fatalf("paused count = %d, err %v; want 0, nil", res.n, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for paused count to finish")
	}
	select {
	case err := <-createDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for NewAccessKey to finish")
	}

	st.countKeysFn = func(context.Context) (int, error) {
		t.Error("later CountAccessKeys hit the database after a successful create")
		return 0, nil
	}
	if n, err := st.CountAccessKeys(ctx); err != nil || n == 0 {
		t.Fatalf("after overlap, count = %d, err %v; want key presence", n, err)
	}
}

// TestCountAccessKeysDeleteOverlapDoesNotKeepStalePresence is the delete-side
// mirror: a count observes one key and pauses, deletion cannot finish until that
// count releases the lock, and the final cache reports zero.
func TestCountAccessKeysDeleteOverlapDoesNotKeepStalePresence(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	key, err := st.NewAccessKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	st.hasKeysMu.Lock()
	st.hasKeys = nil
	st.hasKeysMu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	st.countKeysFn = func(context.Context) (int, error) {
		close(entered)
		<-release
		return 1, nil
	}
	t.Cleanup(func() { st.countKeysFn = nil })

	type countResult struct {
		n   int
		err error
	}
	countDone := make(chan countResult, 1)
	go func() {
		n, err := st.CountAccessKeys(ctx)
		countDone <- countResult{n, err}
	}()
	waitClosed(t, entered, 2*time.Second, "count to enter countKeysFn")

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- st.DeleteAccessKey(ctx, key.ID)
	}()
	assertBlocked(t, deleteDone, 150*time.Millisecond, "DeleteAccessKey")

	close(release)
	select {
	case res := <-countDone:
		if res.err != nil || res.n != 1 {
			t.Fatalf("paused count = %d, err %v; want 1, nil", res.n, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for paused count to finish")
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DeleteAccessKey to finish")
	}

	// Delete invalidates the cache. The final count must use the database, not the paused stub.
	st.countKeysFn = nil
	if n, err := st.CountAccessKeys(ctx); err != nil || n != 0 {
		t.Fatalf("after overlap, count = %d, err %v; want 0, nil", n, err)
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
