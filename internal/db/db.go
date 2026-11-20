// Package db opens trunk's SQLite index and keeps its schema current.
//
// The database is only an index: originals live as plain files on disk, so a
// lost database can be rebuilt from them. Every change that a client should
// see in its delta sync stamps the row with a fresh value from one global
// sequence (see NextSeq).
package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open opens (and creates) the database at path and applies migrations.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY churn and
	// is plenty fast for a household library.
	d.SetMaxOpenConns(1)
	if err := migrate(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT INTO meta (key, value) VALUES ('seq', 0);

CREATE TABLE users (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	pw_hash    TEXT NOT NULL,
	is_admin   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE devices (
	id           INTEGER PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	token_hash   BLOB NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL
);

CREATE TABLE assets (
	id            INTEGER PRIMARY KEY,
	user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	seq           INTEGER NOT NULL,
	sha256        TEXT NOT NULL,
	path          TEXT NOT NULL,          -- relative to the originals dir; '' once deleted
	orig_name     TEXT NOT NULL,
	kind          TEXT NOT NULL,          -- image | video
	mime          TEXT NOT NULL,
	size          INTEGER NOT NULL,
	taken_at      INTEGER NOT NULL,       -- unix ms, UTC instant
	tz_offset     INTEGER,                -- minutes east of UTC, NULL if unknown
	width         INTEGER,
	height        INTEGER,
	orientation   INTEGER,                -- EXIF 1..8
	duration_ms   INTEGER,
	lat           REAL,
	lon           REAL,
	alt           REAL,
	make          TEXT,
	model         TEXT,
	lens          TEXT,
	f_number      REAL,
	exposure      REAL,                   -- seconds
	iso           INTEGER,
	focal_length  REAL,                   -- mm
	favorite      INTEGER NOT NULL DEFAULT 0,
	trashed_at    INTEGER,                -- unix ms
	deleted_at    INTEGER,                -- unix ms; row stays as a tombstone for sync
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX assets_user_sha ON assets(user_id, sha256) WHERE deleted_at IS NULL;
CREATE INDEX assets_user_seq ON assets(user_id, seq);
CREATE INDEX assets_user_taken ON assets(user_id, taken_at) WHERE deleted_at IS NULL;
CREATE INDEX assets_trash ON assets(trashed_at) WHERE trashed_at IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE uploads (
	id         TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	length     INTEGER NOT NULL,
	offset     INTEGER NOT NULL DEFAULT 0,
	filename   TEXT NOT NULL,
	sha256     TEXT NOT NULL DEFAULT '',
	mtime      INTEGER,                   -- unix ms supplied by the client
	asset_id   INTEGER,                   -- set once complete
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`,
}

func migrate(d *sql.DB) error {
	var v int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Execer is satisfied by *sql.DB and *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NextSeq bumps and returns the global change sequence. Call it inside the
// same transaction as the change it stamps, so a client that has seen seq N
// has seen every change up to N.
func NextSeq(ctx context.Context, x Execer) (int64, error) {
	var s int64
	err := x.QueryRowContext(ctx, `UPDATE meta SET value = value + 1 WHERE key = 'seq' RETURNING value`).Scan(&s)
	return s, err
}
