package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
// provider, decrypts its key and OAuth credentials, and appends the target in
// position order.
func (s *Store) hydrateTargets(ctx context.Context, byID map[int64]*domain.Combo) error {
	if len(byID) == 0 {
		return nil
	}
	const q = `
SELECT t.combo_id, t.id, t.provider_id, t.upstream_model, t.position, t.enabled,
       p.id, p.name, p.base_url, p.api_key, p.protocol, p.auth_scheme, p.auth_method, p.oauth_creds, p.created_at, p.updated_at, p.archived
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
		var enc, oauthEnc string
		if err := rows.Scan(
			&comboID, &t.ID, &t.ProviderID, &t.UpstreamModel, &t.Position, &t.Enabled,
			&p.ID, &p.Name, &p.BaseURL, &enc, &p.Protocol, &p.AuthScheme, &p.AuthMethod, &oauthEnc, &p.CreatedAt, &p.UpdatedAt, &p.Archived,
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
		if oauthEnc != "" {
			plain, err := s.cipher.Decrypt(oauthEnc)
			if err != nil {
				return err
			}
			var creds domain.OAuthCreds
			if err := json.Unmarshal([]byte(plain), &creds); err != nil {
				return err
			}
			p.OAuthCreds = &creds
		}
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

// SetTargetEnabled flips a single target's enabled flag without rewriting the
// combo's target set, so the inline list toggle is a cheap one-row update.
func (s *Store) SetTargetEnabled(ctx context.Context, targetID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE combo_targets SET enabled=? WHERE id=?", v, targetID)
	return err
}

// insertTargets writes targets with position set to slice order, so the stored
// position always reflects the caller's intended ordering.
func insertTargets(ctx context.Context, tx *sql.Tx, comboID int64, targets []domain.ComboTarget) error {
	for i, t := range targets {
		enabled := 0
		if t.Enabled {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO combo_targets (combo_id, provider_id, upstream_model, position, enabled) VALUES (?, ?, ?, ?, ?)",
			comboID, t.ProviderID, t.UpstreamModel, i, enabled); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteCombo(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM combos WHERE id = ?", id)
	return err
}

// SwapComboNames atomically exchanges the names of two combos. SQLite enforces
// the combos.name UNIQUE constraint per-statement even inside a transaction, so
// a direct two-way rename fails mid-swap; the exchange instead routes one row
// through a guaranteed-unique temp name within a single tx. Targets, strategy,
// and row ids are untouched (combo ids never move, so combo_targets FKs stay
// valid), and the hot-path GetComboByName sees only the pre- or post-commit
// state, never an intermediate rename.
func (s *Store) SwapComboNames(ctx context.Context, idA, idB int64) error {
	if idA == idB {
		return fmt.Errorf("cannot swap a combo with itself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var nameA, nameB string
	row := tx.QueryRowContext(ctx, "SELECT name FROM combos WHERE id=?", idA)
	if err := row.Scan(&nameA); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: combo %d", ErrNotFound, idA)
		}
		return err
	}
	row = tx.QueryRowContext(ctx, "SELECT name FROM combos WHERE id=?", idB)
	if err := row.Scan(&nameB); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: combo %d", ErrNotFound, idB)
		}
		return err
	}

	// Nanosecond suffix avoids a collision with a combo literally named like the
	// placeholder; the three updates commit atomically so readers never observe it.
	tmp := fmt.Sprintf("__airouter_swap_tmp_%d_%d_%d__", idA, idB, time.Now().UnixNano())
	steps := []struct {
		name string
		id   int64
	}{{tmp, idA}, {nameA, idB}, {nameB, idA}}
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx,
			"UPDATE combos SET name=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", st.name, st.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
