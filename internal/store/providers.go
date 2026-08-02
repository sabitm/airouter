package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"airouter/internal/domain"
)

var ErrNotFound = errors.New("store: not found")

// scanProvider decrypts the stored API key and (when present) OAuth credentials
// into the domain struct.
func (s *Store) scanProvider(row interface{ Scan(...any) error }) (*domain.Provider, error) {
	var p domain.Provider
	var enc, oauthEnc string
	if err := row.Scan(&p.ID, &p.Name, &p.BaseURL, &enc, &p.Protocol, &p.AuthScheme,
		&p.AuthMethod, &oauthEnc, &p.Archived, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	key, err := s.cipher.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	p.APIKey = key
	if oauthEnc != "" {
		plain, err := s.cipher.Decrypt(oauthEnc)
		if err != nil {
			return nil, err
		}
		var creds domain.OAuthCreds
		if err := json.Unmarshal([]byte(plain), &creds); err != nil {
			return nil, err
		}
		p.OAuthCreds = &creds
	}
	return &p, nil
}

const providerCols = "id, name, base_url, api_key, protocol, auth_scheme, auth_method, oauth_creds, archived, created_at, updated_at"

func (s *Store) ListProviders(ctx context.Context) ([]*domain.Provider, error) {
	return s.listProviders(ctx, s.db)
}

// listProviders is the executor-parameterised body.
func (s *Store) listProviders(ctx context.Context, ex executor) ([]*domain.Provider, error) {
	rows, err := ex.QueryContext(ctx, "SELECT "+providerCols+" FROM providers ORDER BY name")
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

// encryptOAuth returns the encrypted JSON of creds, or "" when nil.
func (s *Store) encryptOAuth(creds *domain.OAuthCreds) (string, error) {
	if creds == nil {
		return "", nil
	}
	b, err := json.Marshal(creds)
	if err != nil {
		return "", err
	}
	return s.cipher.Encrypt(string(b))
}

func (s *Store) CreateProvider(ctx context.Context, p *domain.Provider) error {
	return s.createProvider(ctx, s.db, p)
}

// createProvider is the executor-parameterised body; ex is s.db normally and
// the import *sql.Tx during a transactional Import.
func (s *Store) createProvider(ctx context.Context, ex executor, p *domain.Provider) error {
	enc, err := s.cipher.Encrypt(p.APIKey)
	if err != nil {
		return err
	}
	oauthEnc, err := s.encryptOAuth(p.OAuthCreds)
	if err != nil {
		return err
	}
	res, err := ex.ExecContext(ctx,
		"INSERT INTO providers (name, base_url, api_key, protocol, auth_scheme, auth_method, oauth_creds, archived) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		p.Name, p.BaseURL, enc, p.Protocol, p.AuthScheme, p.AuthMethod, oauthEnc, p.Archived)
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	return err
}

func (s *Store) UpdateProvider(ctx context.Context, p *domain.Provider) error {
	return s.updateProvider(ctx, s.db, p)
}

// updateProvider is the executor-parameterised body; see createProvider.
func (s *Store) updateProvider(ctx context.Context, ex executor, p *domain.Provider) error {
	enc, err := s.cipher.Encrypt(p.APIKey)
	if err != nil {
		return err
	}
	oauthEnc, err := s.encryptOAuth(p.OAuthCreds)
	if err != nil {
		return err
	}
	_, err = ex.ExecContext(ctx,
		"UPDATE providers SET name=?, base_url=?, api_key=?, protocol=?, auth_scheme=?, auth_method=?, oauth_creds=?, archived=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		p.Name, p.BaseURL, enc, p.Protocol, p.AuthScheme, p.AuthMethod, oauthEnc, p.Archived, p.ID)
	return err
}

// UpdateProviderOAuth refreshes only the OAuth credentials for a provider,
// leaving the rest of the row (name, base URL, protocol, key) untouched. Used by
// the token refresh path, which must persist a rotated token without clobbering
// unrelated fields a concurrent dashboard edit might be changing.
func (s *Store) UpdateProviderOAuth(ctx context.Context, id int64, creds *domain.OAuthCreds) error {
	oauthEnc, err := s.encryptOAuth(creds)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"UPDATE providers SET oauth_creds=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		oauthEnc, id)
	return err
}

func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM providers WHERE id = ?", id)
	return err
}

// SetProviderArchived flips a provider's archived flag without touching the
// rest of the row, so archive/restore is a cheap one-row update.
func (s *Store) SetProviderArchived(ctx context.Context, id int64, archived bool) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE providers SET archived=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", archived, id)
	return err
}

// DeleteArchivedProviders hard-deletes every archived provider. Foreign-key
// cascade removes their combo_targets.
func (s *Store) DeleteArchivedProviders(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM providers WHERE archived=1")
	return err
}
