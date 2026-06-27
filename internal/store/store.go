package store

import (
	"context"
	"database/sql"
	"fmt"

	"airouter/internal/crypto"

	_ "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

func Open(path string, cipher *crypto.Cipher) (*Store, error) {
	// _pragma busy_timeout avoids spurious "database is locked" under the
	// dashboard's concurrent reads/writes; foreign_keys must be enabled per
	// connection for ON DELETE to apply.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: db, cipher: cipher}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS providers (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	base_url    TEXT NOT NULL,
	api_key     TEXT NOT NULL,
	protocol    TEXT NOT NULL,
	auth_scheme TEXT NOT NULL DEFAULT '',
	auth_method TEXT NOT NULL DEFAULT '',
	oauth_creds TEXT NOT NULL DEFAULT '',
	created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS combos (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	strategy   TEXT NOT NULL DEFAULT 'failover',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS combo_targets (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	combo_id       INTEGER NOT NULL REFERENCES combos(id) ON DELETE CASCADE,
	provider_id    INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	upstream_model TEXT NOT NULL,
	position       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_combo_targets_combo ON combo_targets(combo_id, position);

CREATE TABLE IF NOT EXISTS access_keys (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	prefix     TEXT NOT NULL,
	hash       TEXT NOT NULL UNIQUE,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS request_logs (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	access_key_name TEXT NOT NULL DEFAULT '',
	combo           TEXT NOT NULL DEFAULT '',
	provider        TEXT NOT NULL DEFAULT '',
	upstream_model  TEXT NOT NULL DEFAULT '',
	format          TEXT NOT NULL DEFAULT '',
	stream          INTEGER NOT NULL DEFAULT 0,
	status          INTEGER NOT NULL DEFAULT 0,
	input_tokens    INTEGER NOT NULL DEFAULT 0,
	output_tokens   INTEGER NOT NULL DEFAULT 0,
	latency_ms      INTEGER NOT NULL DEFAULT 0,
	err_msg         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at DESC);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if err := s.migrateProviderAuthScheme(); err != nil {
		return err
	}
	if err := s.migrateProviderAuthMethod(); err != nil {
		return err
	}
	return s.migrateCombosToTargets()
}

// migrateProviderAuthScheme adds the auth_scheme column to a providers table
// created before auth was decoupled from protocol. Idempotent; existing rows
// default to '' (empty), which Provider.Auth resolves by protocol.
func (s *Store) migrateProviderAuthScheme() error {
	has, err := s.columnExists("providers", "auth_scheme")
	if err != nil || has {
		return err
	}
	_, err = s.db.Exec("ALTER TABLE providers ADD COLUMN auth_scheme TEXT NOT NULL DEFAULT ''")
	return err
}

// migrateProviderAuthMethod adds the auth_method and oauth_creds columns to a
// providers table created before OAuth was supported. Idempotent; existing rows
// default to '' (empty), which Provider.Method resolves to apikey.
func (s *Store) migrateProviderAuthMethod() error {
	has, err := s.columnExists("providers", "auth_method")
	if err != nil || has {
		return err
	}
	if _, err := s.db.Exec("ALTER TABLE providers ADD COLUMN auth_method TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err = s.db.Exec("ALTER TABLE providers ADD COLUMN oauth_creds TEXT NOT NULL DEFAULT ''")
	return err
}

// migrateCombosToTargets upgrades the pre-multi-target schema: when the combos
// table still carries the legacy provider_id/upstream_model columns, each combo
// is backfilled as a single position-0 target and the combos table is rebuilt
// to the current shape. Idempotent: a no-op once the legacy columns are gone.
func (s *Store) migrateCombosToTargets() error {
	hasLegacy, err := s.columnExists("combos", "provider_id")
	if err != nil {
		return err
	}
	if !hasLegacy {
		return nil
	}

	// The rebuild drops the old combos table. With foreign keys enabled, that
	// drop would implicitly delete child rows and cascade into combo_targets,
	// wiping the just-backfilled rows; disable enforcement for the rebuild. The
	// connection is restored to foreign_keys=ON before returning to the pool.
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON")
		conn.Close()
	}()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`INSERT INTO combo_targets (combo_id, provider_id, upstream_model, position)
		 SELECT id, provider_id, upstream_model, 0 FROM combos
		 WHERE id NOT IN (SELECT combo_id FROM combo_targets)`,
		`CREATE TABLE combos_new (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL UNIQUE,
			strategy   TEXT NOT NULL DEFAULT 'failover',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO combos_new (id, name, strategy, created_at, updated_at)
		 SELECT id, name, 'failover', created_at, updated_at FROM combos`,
		`DROP TABLE combos`,
		`ALTER TABLE combos_new RENAME TO combos`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) columnExists(table, col string) (bool, error) {
	rows, err := s.db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}
