package store

import (
	"context"
	"database/sql"
	"errors"

	"airouter/internal/domain"
)

// listCombos joins providers so the combo list can display the bound provider.
// The provider API key is decrypted here too, since combo resolution needs it.
func (s *Store) ListCombos(ctx context.Context) ([]*domain.Combo, error) {
	const q = `
SELECT c.id, c.name, c.provider_id, c.upstream_model, c.created_at, c.updated_at,
       p.id, p.name, p.base_url, p.api_key, p.protocol, p.created_at, p.updated_at
FROM combos c JOIN providers p ON p.id = c.provider_id
ORDER BY c.name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Combo
	for rows.Next() {
		c, err := s.scanComboWithProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) scanComboWithProvider(row interface{ Scan(...any) error }) (*domain.Combo, error) {
	var c domain.Combo
	var p domain.Provider
	var enc string
	if err := row.Scan(
		&c.ID, &c.Name, &c.ProviderID, &c.UpstreamModel, &c.CreatedAt, &c.UpdatedAt,
		&p.ID, &p.Name, &p.BaseURL, &enc, &p.Protocol, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	key, err := s.cipher.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	p.APIKey = key
	c.Provider = &p
	return &c, nil
}

// GetComboByName resolves a custom model name to its combo + provider. This is
// the hot path used by the proxy.
func (s *Store) GetComboByName(ctx context.Context, name string) (*domain.Combo, error) {
	const q = `
SELECT c.id, c.name, c.provider_id, c.upstream_model, c.created_at, c.updated_at,
       p.id, p.name, p.base_url, p.api_key, p.protocol, p.created_at, p.updated_at
FROM combos c JOIN providers p ON p.id = c.provider_id
WHERE c.name = ?`
	row := s.db.QueryRowContext(ctx, q, name)
	c, err := s.scanComboWithProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *Store) GetCombo(ctx context.Context, id int64) (*domain.Combo, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT id, name, provider_id, upstream_model, created_at, updated_at FROM combos WHERE id = ?", id)
	var c domain.Combo
	err := row.Scan(&c.ID, &c.Name, &c.ProviderID, &c.UpstreamModel, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) CreateCombo(ctx context.Context, c *domain.Combo) error {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO combos (name, provider_id, upstream_model) VALUES (?, ?, ?)",
		c.Name, c.ProviderID, c.UpstreamModel)
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *Store) UpdateCombo(ctx context.Context, c *domain.Combo) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE combos SET name=?, provider_id=?, upstream_model=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		c.Name, c.ProviderID, c.UpstreamModel, c.ID)
	return err
}

func (s *Store) DeleteCombo(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM combos WHERE id = ?", id)
	return err
}
