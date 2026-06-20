package store

import (
	"context"
	"database/sql"
	"errors"

	"airouter/internal/domain"
)

// ListCombos returns all combos with their ordered targets, each target's
// provider hydrated (API key decrypted) for display and resolution.
func (s *Store) ListCombos(ctx context.Context) ([]*domain.Combo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, strategy, created_at, updated_at FROM combos ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Combo
	byID := map[int64]*domain.Combo{}
	for rows.Next() {
		var c domain.Combo
		if err := rows.Scan(&c.ID, &c.Name, &c.Strategy, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
		byID[c.ID] = &c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.hydrateTargets(ctx, byID); err != nil {
		return nil, err
	}
	return out, nil
}

// hydrateTargets loads every combo_targets row whose combo is in byID, joins the
// provider, decrypts its key, and appends the target in position order.
func (s *Store) hydrateTargets(ctx context.Context, byID map[int64]*domain.Combo) error {
	if len(byID) == 0 {
		return nil
	}
	const q = `
SELECT t.combo_id, t.id, t.provider_id, t.upstream_model, t.position,
       p.id, p.name, p.base_url, p.api_key, p.protocol, p.auth_scheme, p.created_at, p.updated_at
FROM combo_targets t JOIN providers p ON p.id = t.provider_id
ORDER BY t.combo_id, t.position, t.id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var comboID int64
		var t domain.ComboTarget
		var p domain.Provider
		var enc string
		if err := rows.Scan(
			&comboID, &t.ID, &t.ProviderID, &t.UpstreamModel, &t.Position,
			&p.ID, &p.Name, &p.BaseURL, &enc, &p.Protocol, &p.AuthScheme, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return err
		}
		c, ok := byID[comboID]
		if !ok {
			continue
		}
		key, err := s.cipher.Decrypt(enc)
		if err != nil {
			return err
		}
		p.APIKey = key
		t.Provider = &p
		c.Targets = append(c.Targets, t)
	}
	return rows.Err()
}

// GetComboByName resolves a custom model name to its combo + ordered targets.
// This is the hot path used by the proxy.
func (s *Store) GetComboByName(ctx context.Context, name string) (*domain.Combo, error) {
	var c domain.Combo
	row := s.db.QueryRowContext(ctx,
		"SELECT id, name, strategy, created_at, updated_at FROM combos WHERE name = ?", name)
	if err := row.Scan(&c.ID, &c.Name, &c.Strategy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.hydrateTargets(ctx, map[int64]*domain.Combo{c.ID: &c}); err != nil {
		return nil, err
	}
	return &c, nil
}

// GetCombo loads a combo and its targets by id.
func (s *Store) GetCombo(ctx context.Context, id int64) (*domain.Combo, error) {
	var c domain.Combo
	row := s.db.QueryRowContext(ctx,
		"SELECT id, name, strategy, created_at, updated_at FROM combos WHERE id = ?", id)
	if err := row.Scan(&c.ID, &c.Name, &c.Strategy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.hydrateTargets(ctx, map[int64]*domain.Combo{c.ID: &c}); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) CreateCombo(ctx context.Context, c *domain.Combo) error {
	if c.Strategy == "" {
		c.Strategy = domain.StrategyFailover
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		"INSERT INTO combos (name, strategy) VALUES (?, ?)", c.Name, c.Strategy)
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	if err != nil {
		return err
	}
	if err := insertTargets(ctx, tx, c.ID, c.Targets); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateCombo updates the combo metadata and replaces its target rows wholesale.
func (s *Store) UpdateCombo(ctx context.Context, c *domain.Combo) error {
	if c.Strategy == "" {
		c.Strategy = domain.StrategyFailover
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		"UPDATE combos SET name=?, strategy=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		c.Name, c.Strategy, c.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM combo_targets WHERE combo_id=?", c.ID); err != nil {
		return err
	}
	if err := insertTargets(ctx, tx, c.ID, c.Targets); err != nil {
		return err
	}
	return tx.Commit()
}

// insertTargets writes targets with position set to slice order, so the stored
// position always reflects the caller's intended ordering.
func insertTargets(ctx context.Context, tx *sql.Tx, comboID int64, targets []domain.ComboTarget) error {
	for i, t := range targets {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO combo_targets (combo_id, provider_id, upstream_model, position) VALUES (?, ?, ?, ?)",
			comboID, t.ProviderID, t.UpstreamModel, i); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteCombo(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM combos WHERE id = ?", id)
	return err
}
