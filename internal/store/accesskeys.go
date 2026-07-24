package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"airouter/internal/domain"
)

const tokenPrefix = "sk-air-"

// NewAccessKey generates a random token, persists only its hash + display
// prefix, and returns the domain object with the raw Token set (shown once).
func (s *Store) NewAccessKey(ctx context.Context, name string) (*domain.AccessKey, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := tokenPrefix + hex.EncodeToString(raw)
	hash := hashToken(token)
	display := token[:len(tokenPrefix)+6] + "..."

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO access_keys (name, prefix, hash) VALUES (?, ?, ?)",
		name, display, hash)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	s.invalidateHasKeys()
	return &domain.AccessKey{ID: id, Name: name, Prefix: display, Hash: hash, Token: token}, nil
}

func (s *Store) ListAccessKeys(ctx context.Context) ([]*domain.AccessKey, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, prefix, hash, created_at FROM access_keys ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.AccessKey
	for rows.Next() {
		var k domain.AccessKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// CountAccessKeys returns the number of access keys. When zero, the proxy
// runs in open mode and accepts unauthenticated requests. It is a DB hot path
// on every unauthenticated request, so the result is cached and only recomputed
// when the cache is unknown or after a create/delete invalidates it.
func (s *Store) CountAccessKeys(ctx context.Context) (int, error) {
	s.hasKeysMu.RLock()
	p := s.hasKeys
	s.hasKeysMu.RUnlock()
	if p != nil {
		if *p {
			return 1, nil // keys exist; the exact count is irrelevant for the open-mode gate
		}
		return 0, nil
	}
	n, err := s.countKeys(ctx)
	if err != nil {
		return 0, err
	}
	present := n > 0
	s.hasKeysMu.Lock()
	s.hasKeys = &present
	s.hasKeysMu.Unlock()
	return n, nil
}

// countKeys delegates to countKeysFn when set (tests), else hits the DB.
func (s *Store) countKeys(ctx context.Context) (int, error) {
	if s.countKeysFn != nil {
		return s.countKeysFn(ctx)
	}
	return s.countAccessKeysDB(ctx)
}

// countAccessKeysDB hits the DB and is only called on a cache miss.
func (s *Store) countAccessKeysDB(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM access_keys").Scan(&n)
	return n, err
}

// invalidateHasKeys clears the cached key-presence flag so the next
// CountAccessKeys recomputes it. Called after a create or delete.
func (s *Store) invalidateHasKeys() {
	s.hasKeysMu.Lock()
	s.hasKeys = nil
	s.hasKeysMu.Unlock()
}

// VerifyToken returns the matching access key for a raw bearer token, or
// ErrNotFound. Used by the proxy auth middleware.
func (s *Store) VerifyToken(ctx context.Context, token string) (*domain.AccessKey, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT id, name, prefix, hash, created_at FROM access_keys WHERE hash = ?", hashToken(token))
	var k domain.AccessKey
	err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &k, err
}

func (s *Store) DeleteAccessKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM access_keys WHERE id = ?", id)
	if err == nil {
		s.invalidateHasKeys()
	}
	return err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
