package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"airouter/internal/domain"
)

var ErrNotFound = errors.New("store: not found")

// ErrConflict reports that a conditional write found a newer or incompatible
// row. The operation writes nothing. Callers must not treat this as success
// or as proof that the original credentials were rejected by an upstream.
var ErrConflict = errors.New("store: provider changed during update")

// scanProvider decrypts the stored API key and (when present) OAuth credentials
// into the domain struct.
func (s *Store) scanProvider(row interface{ Scan(...any) error }) (*domain.Provider, error) {
	p, _, err := s.scanProviderStored(row)
	return p, err
}

// scanProviderStored is scanProvider plus the exact encrypted oauth_creds blob
// from the same row read. Conditional saves compare that blob; a fresh
// encryption of identical plaintext is not equal because nonces differ.
func (s *Store) scanProviderStored(row interface{ Scan(...any) error }) (*domain.Provider, string, error) {
	var p domain.Provider
	var enc, oauthEnc, dialect, tagsJSON string
	if err := row.Scan(&p.ID, &p.Name, &p.BaseURL, &enc, &p.Protocol, &p.AuthScheme,
		&p.AuthMethod, &oauthEnc, &dialect, &tagsJSON, &p.Archived, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, "", err
	}
	p.ReasoningDialect = domain.ReasoningDialect(dialect)
	tags, err := decodeProviderTags(tagsJSON)
	if err != nil {
		return nil, "", err
	}
	p.Tags = tags
	key, err := s.cipher.Decrypt(enc)
	if err != nil {
		return nil, "", err
	}
	p.APIKey = key
	if oauthEnc != "" {
		plain, err := s.cipher.Decrypt(oauthEnc)
		if err != nil {
			return nil, "", err
		}
		var creds domain.OAuthCreds
		if err := json.Unmarshal([]byte(plain), &creds); err != nil {
			return nil, "", err
		}
		p.OAuthCreds = &creds
	}
	return &p, oauthEnc, nil
}

const providerCols = "id, name, base_url, api_key, protocol, auth_scheme, auth_method, oauth_creds, reasoning_dialect, tags, archived, created_at, updated_at"

func encodeProviderTags(tags []string) (string, error) {
	norm, err := domain.NormalizeTags(tags)
	if err != nil {
		return "", err
	}
	if norm == nil {
		norm = []string{}
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeProviderTags(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, err
	}
	return domain.NormalizeTags(tags)
}

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

// ProviderOAuthStored is a provider plus the exact encrypted oauth_creds blob
// read from the row. Conditional saves compare that blob. They must not
// compare a newly encrypted copy, because each encryption uses a new nonce.
type ProviderOAuthStored struct {
	Provider  *domain.Provider
	OAuthBlob string
}

// GetProviderOAuthStored reads one provider and its current oauth_creds blob.
// A missing row is ErrNotFound.
func (s *Store) GetProviderOAuthStored(ctx context.Context, id int64) (*ProviderOAuthStored, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+providerCols+" FROM providers WHERE id = ?", id)
	p, oauthEnc, err := s.scanProviderStored(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ProviderOAuthStored{Provider: p, OAuthBlob: oauthEnc}, nil
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
	tagsJSON, err := encodeProviderTags(p.Tags)
	if err != nil {
		return err
	}
	res, err := ex.ExecContext(ctx,
		"INSERT INTO providers (name, base_url, api_key, protocol, auth_scheme, auth_method, oauth_creds, reasoning_dialect, tags, archived) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p.Name, p.BaseURL, enc, p.Protocol, p.AuthScheme, p.AuthMethod, oauthEnc, p.ReasoningDialect, tagsJSON, p.Archived)
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	return err
}

func (s *Store) UpdateProvider(ctx context.Context, p *domain.Provider) error {
	return s.updateProvider(ctx, s.db, p)
}

// ProviderMeta is the form-controlled portion of a provider row. archived is
// excluded: it is not form state, and a stale snapshot must not restore it over
// a concurrent archive or restore.
type ProviderMeta struct {
	Name             string
	BaseURL          string
	APIKey           string
	Protocol         domain.Protocol
	AuthScheme       domain.AuthScheme
	AuthMethod       domain.AuthMethod
	ReasoningDialect domain.ReasoningDialect
	Tags             []string
}

// ProviderConnectionSave is a provider save that also replaces oauth_creds.
// archived is excluded for the same reason as ProviderMeta.
type ProviderConnectionSave struct {
	ProviderMeta
	OAuthCreds *domain.OAuthCreds
}

// UpdateProviderMetaIfConnection updates provider metadata and leaves
// oauth_creds untouched, so a concurrent token rotation or reconnect is not
// restored from a stale snapshot. The write is conditional on the connection
// state read before the form save, so a concurrent auth method or protocol
// switch is not mixed with this form. A missing row is ErrNotFound. A changed
// connection is ErrConflict. Zero affected rows are never reported as success.
func (s *Store) UpdateProviderMetaIfConnection(ctx context.Context, id int64, meta ProviderMeta, method domain.AuthMethod, proto domain.Protocol) error {
	enc, err := s.cipher.Encrypt(meta.APIKey)
	if err != nil {
		return err
	}
	tagsJSON, err := encodeProviderTags(meta.Tags)
	if err != nil {
		return err
	}
	return s.conditionalUpdate(ctx,
		"UPDATE providers SET name=?, base_url=?, api_key=?, protocol=?, auth_scheme=?, auth_method=?, reasoning_dialect=?, tags=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND auth_method=? AND protocol=?",
		[]any{meta.Name, meta.BaseURL, enc, meta.Protocol, meta.AuthScheme, meta.AuthMethod, meta.ReasoningDialect, tagsJSON, id, method, proto},
		id)
}

// UpdateProviderIfConnection writes provider fields and a coherent credential
// snapshot only while the stored encrypted credential blob and the connection
// state are still the ones the caller observed before the token call. A fresh
// encryption of the same plaintext is not used for equality, because encryption
// nonces differ. A missing row is ErrNotFound. A changed blob or connection is
// ErrConflict. No partial update is committed.
func (s *Store) UpdateProviderIfConnection(ctx context.Context, id int64, save ProviderConnectionSave, method domain.AuthMethod, proto domain.Protocol, expectedOAuth string) error {
	enc, err := s.cipher.Encrypt(save.APIKey)
	if err != nil {
		return err
	}
	oauthEnc, err := s.encryptOAuth(save.OAuthCreds)
	if err != nil {
		return err
	}
	tagsJSON, err := encodeProviderTags(save.Tags)
	if err != nil {
		return err
	}
	return s.conditionalUpdate(ctx,
		"UPDATE providers SET name=?, base_url=?, api_key=?, protocol=?, auth_scheme=?, auth_method=?, oauth_creds=?, reasoning_dialect=?, tags=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND oauth_creds=? AND auth_method=? AND protocol=?",
		[]any{save.Name, save.BaseURL, enc, save.Protocol, save.AuthScheme, save.AuthMethod, oauthEnc, save.ReasoningDialect, tagsJSON, id, expectedOAuth, method, proto},
		id)
}

// conditionalUpdate commits one guarded write. The existence check and the
// update share a transaction, so a deleted row is not reported as a credential
// conflict and a changed predicate is not reported as success. The transaction
// rolls back unless exactly one row changed.
func (s *Store) conditionalUpdate(ctx context.Context, query string, args []any, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM providers WHERE id = ?", id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	if n != 1 {
		return fmt.Errorf("store: conditional update affected %d rows", n)
	}
	return tx.Commit()
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
	tagsJSON, err := encodeProviderTags(p.Tags)
	if err != nil {
		return err
	}
	_, err = ex.ExecContext(ctx,
		"UPDATE providers SET name=?, base_url=?, api_key=?, protocol=?, auth_scheme=?, auth_method=?, oauth_creds=?, reasoning_dialect=?, tags=?, archived=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		p.Name, p.BaseURL, enc, p.Protocol, p.AuthScheme, p.AuthMethod, oauthEnc, p.ReasoningDialect, tagsJSON, p.Archived, p.ID)
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

// UpdateProviderOAuthCAS applies a conditional read-modify-write to one OAuth
// credential blob. The update callback runs against the current decrypted
// snapshot. A write succeeds only if the stored encrypted blob is still the
// one the callback saw, so a refresh cannot overwrite a reconnect or another
// writer that committed between the read and the update. The callback may run
// more than once; it must derive its result from the argument, not from
// captured mutable state.
func (s *Store) UpdateProviderOAuthCAS(ctx context.Context, id int64, update func(current *domain.OAuthCreds) (*domain.OAuthCreds, bool, error)) (*domain.OAuthCreds, error) {
	if update == nil {
		return nil, errors.New("store: nil oauth update")
	}
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var stored string
		err := s.db.QueryRowContext(ctx, "SELECT oauth_creds FROM providers WHERE id = ?", id).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		current, err := s.decryptOAuth(stored)
		if err != nil {
			return nil, err
		}
		base, err := s.decryptOAuth(stored)
		if err != nil {
			return nil, err
		}
		next, changed, err := update(current)
		if err != nil {
			return nil, err
		}
		if !changed {
			return base, nil
		}
		oauthEnc, err := s.encryptOAuth(next)
		if err != nil {
			return nil, err
		}
		res, err := s.db.ExecContext(ctx,
			"UPDATE providers SET oauth_creds=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND oauth_creds=?",
			oauthEnc, id, stored)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			out := *next
			return &out, nil
		}
		// The blob changed after the read. Loop so the callback sees the newest
		// token, account, and device instead of overwriting them.
	}
	return nil, errors.New("store: oauth compare-and-swap did not converge")
}

// UpdateProviderConnectionCAS applies a conditional read-modify-write to one
// OAuth credential blob, guarded additionally by the provider auth method and
// protocol observed at the same read. The callback receives the current
// connection state so it can classify a switch instead of writing into a row
// that changed connection (for example Claude Code to Anthropic) after the
// caller's snapshot. A missing row is ErrNotFound. The callback may run more
// than once and must derive its result from its arguments, not from captured
// mutable state.
func (s *Store) UpdateProviderConnectionCAS(ctx context.Context, id int64, update func(current *domain.OAuthCreds, method domain.AuthMethod, proto domain.Protocol) (*domain.OAuthCreds, bool, error)) (*domain.OAuthCreds, error) {
	if update == nil {
		return nil, errors.New("store: nil oauth update")
	}
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var stored, authMethod, protocol string
		err := s.db.QueryRowContext(ctx, "SELECT oauth_creds, auth_method, protocol FROM providers WHERE id = ?", id).Scan(&stored, &authMethod, &protocol)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		current, err := s.decryptOAuth(stored)
		if err != nil {
			return nil, err
		}
		base, err := s.decryptOAuth(stored)
		if err != nil {
			return nil, err
		}
		next, changed, err := update(current, domain.AuthMethod(authMethod), domain.Protocol(protocol))
		if err != nil {
			return nil, err
		}
		if !changed {
			return base, nil
		}
		oauthEnc, err := s.encryptOAuth(next)
		if err != nil {
			return nil, err
		}
		res, err := s.db.ExecContext(ctx,
			"UPDATE providers SET oauth_creds=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND oauth_creds=? AND auth_method=? AND protocol=?",
			oauthEnc, id, stored, authMethod, protocol)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			out := *next
			return &out, nil
		}
		// The blob or connection changed after the read. Loop so the callback
		// sees the newest row instead of overwriting it; a switched connection is
		// then rejected by the callback.
	}
	return nil, errors.New("store: oauth connection compare-and-swap did not converge")
}

func (s *Store) decryptOAuth(stored string) (*domain.OAuthCreds, error) {
	if stored == "" {
		return nil, nil
	}
	plain, err := s.cipher.Decrypt(stored)
	if err != nil {
		return nil, err
	}
	var creds domain.OAuthCreds
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		return nil, err
	}
	return &creds, nil
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
