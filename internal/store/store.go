package store

import (
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
	base_url   TEXT NOT NULL,
	api_key    TEXT NOT NULL,
	protocol   TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS combos (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	name           TEXT NOT NULL UNIQUE,
	provider_id    INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	upstream_model TEXT NOT NULL,
	created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

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
	_, err := s.db.Exec(schema)
	return err
}
