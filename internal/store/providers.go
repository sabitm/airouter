package store

import (
	"context"
	"database/sql"
	"errors"

	"airouter/internal/domain"
)

var ErrNotFound = errors.New("store: not found")

// scanProvider decrypts the stored API key into the domain struct.
func (s *Store) scanProvider(row interface{ Scan(...any) error }) (*domain.Provider, error) {
	var p domain.Provider
	var enc string
	if err := row.Scan(&p.ID, &p.Name, &p.BaseURL, &enc, &p.Protocol, &p.AuthScheme, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	key, err := s.cipher.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	p.APIKey = key
	return &p, nil
}

const providerCols = "id, name, base_url, api_key, protocol, auth_scheme, created_at, updated_at"

func (s *Store) ListProviders(ctx context.Context) ([]*domain.Provider, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+providerCols+" FROM providers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Provider
	for rows.Next() {
		p, err := s.scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProvider(ctx context.Context, id int64) (*domain.Provider, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+providerCols+" FROM providers WHERE id = ?", id)
	p, err := s.scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) CreateProvider(ctx context.Context, p *domain.Provider) error {
	enc, err := s.cipher.Encrypt(p.APIKey)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO providers (name, base_url, api_key, protocol, auth_scheme) VALUES (?, ?, ?, ?, ?)",
		p.Name, p.BaseURL, enc, p.Protocol, p.AuthScheme)
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	return err
}

func (s *Store) UpdateProvider(ctx context.Context, p *domain.Provider) error {
	enc, err := s.cipher.Encrypt(p.APIKey)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"UPDATE providers SET name=?, base_url=?, api_key=?, protocol=?, auth_scheme=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		p.Name, p.BaseURL, enc, p.Protocol, p.AuthScheme, p.ID)
	return err
}

func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM providers WHERE id = ?", id)
	return err
}
