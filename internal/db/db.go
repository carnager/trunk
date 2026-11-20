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
	// 2: edit versions, and every hash an asset is known by for deduplication
	`
-- The assets.sha256/path/size/mime/width/height/orientation columns describe the
-- current file. Once an asset is edited, orig_* keep the first upload.
ALTER TABLE assets ADD COLUMN orig_path TEXT;
ALTER TABLE assets ADD COLUMN orig_sha256 TEXT;
ALTER TABLE assets ADD COLUMN orig_size INTEGER;
ALTER TABLE assets ADD COLUMN orig_mime TEXT;
ALTER TABLE assets ADD COLUMN orig_width INTEGER;
ALTER TABLE assets ADD COLUMN orig_height INTEGER;
ALTER TABLE assets ADD COLUMN orig_orientation INTEGER;
ALTER TABLE assets ADD COLUMN edited_at INTEGER;

-- kind: original (first upload), source (the client's file before it shrank it),
-- edit (the current edited version).
CREATE TABLE asset_hashes (
	user_id  INTEGER NOT NULL,
	sha256   TEXT NOT NULL,
	asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
	kind     TEXT NOT NULL,
	PRIMARY KEY (user_id, sha256)
);
CREATE INDEX asset_hashes_asset ON asset_hashes(asset_id);
INSERT INTO asset_hashes (user_id, sha256, asset_id, kind)
	SELECT user_id, sha256, id, 'original' FROM assets WHERE deleted_at IS NULL;

-- Uniqueness now lives in asset_hashes; an edit may match no other rule.
DROP INDEX assets_user_sha;
CREATE INDEX assets_sha ON assets(sha256) WHERE deleted_at IS NULL;

ALTER TABLE uploads ADD COLUMN source_sha256 TEXT NOT NULL DEFAULT '';
`,
	// 3: albums
	`
CREATE TABLE albums (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	deleted_at INTEGER                    -- tombstone for sync
);
CREATE INDEX albums_user_seq ON albums(user_id, seq);

-- pos orders the album; smaller comes first. Adding to the front uses
-- positions below the current minimum, so nothing has to be renumbered.
CREATE TABLE album_assets (
	album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
	asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
	pos      INTEGER NOT NULL,
	added_at INTEGER NOT NULL,
	PRIMARY KEY (album_id, asset_id)
);
CREATE INDEX album_assets_asset ON album_assets(asset_id);
`,
	// 4: place names and captions
	`
-- GeoNames id of the nearest town; NULL = not looked up yet, 0 = none near.
ALTER TABLE assets ADD COLUMN place_id INTEGER;
ALTER TABLE assets ADD COLUMN caption TEXT;
CREATE INDEX assets_place ON assets(place_id) WHERE place_id > 0;
`,
	// 5: albums shared within the household
	`
-- seq marks the last change of the membership, so joining and leaving reach
-- the member's feed; removed_at keeps a left membership as a tombstone.
CREATE TABLE album_members (
	album_id   INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	can_add    INTEGER NOT NULL DEFAULT 1,
	seq        INTEGER NOT NULL,
	removed_at INTEGER,
	PRIMARY KEY (album_id, user_id)
);
CREATE INDEX album_members_user ON album_members(user_id);
-- Who put a photo into an album: members may take out only what they added.
ALTER TABLE album_assets ADD COLUMN added_by INTEGER;
UPDATE album_assets SET added_by = (SELECT user_id FROM albums WHERE albums.id = album_assets.album_id);
`,
	// 6: photos sent directly to someone in the household
	`
-- album: an ordinary album. direct: the photos one person sent another,
-- one per pair, shared with the recipient who cannot add to it.
ALTER TABLE albums ADD COLUMN kind TEXT NOT NULL DEFAULT 'album';
`,
	// 7: public links for people without an account
	`
-- A link shows either an album (as it is now) or a fixed selection of photos.
CREATE TABLE links (
	token      TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	album_id   INTEGER REFERENCES albums(id) ON DELETE CASCADE,
	title      TEXT NOT NULL,
	download   INTEGER NOT NULL DEFAULT 1,  -- whether originals may be downloaded
	created_at INTEGER NOT NULL,
	expires_at INTEGER,
	revoked_at INTEGER
);
CREATE INDEX links_user ON links(user_id);
CREATE TABLE link_assets (
	token    TEXT NOT NULL REFERENCES links(token) ON DELETE CASCADE,
	asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
	pos      INTEGER NOT NULL,
	PRIMARY KEY (token, asset_id)
);
`,
	// 8: links always allow downloads (browsers can save what they show anyway);
	// what they can leave out is where the photos were taken.
	`
ALTER TABLE links ADD COLUMN strip_location INTEGER NOT NULL DEFAULT 1;
`,
	// 9: look-alike finder. A fingerprint per file content (a 512-bit dHash
	// of its thumbnail), and pairs a user said are not duplicates.
	`
CREATE TABLE fingerprints (
	sha256 TEXT PRIMARY KEY,
	hash   BLOB NOT NULL,
	detail REAL NOT NULL
);
CREATE TABLE similar_ignored (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	a       INTEGER NOT NULL,
	b       INTEGER NOT NULL,
	PRIMARY KEY (user_id, a, b)
);
`,
	// 10: motion photos: where the video sits in the (first) file. NULL until
	// the file was looked at, 0 for a still.
	`
ALTER TABLE assets ADD COLUMN motion_offset INTEGER;
ALTER TABLE assets ADD COLUMN motion_length INTEGER;
`,
	// 11: a motion photo's video kept as a file of its own (when an export
	// had split it off), next to the photo: motion_path, the whole file.
	`
ALTER TABLE assets ADD COLUMN motion_path TEXT;
`,
	// 12: the motion video's SHA-256, to know a video that is only a copy of it.
	`
ALTER TABLE assets ADD COLUMN motion_sha256 TEXT;
CREATE INDEX assets_motion_sha ON assets (user_id, motion_sha256) WHERE motion_sha256 IS NOT NULL;
`,
	// 13: content search. A vector per file content and model, computed by
	// an indexer elsewhere (trunk-ai); little-endian float32s, unit length.
	`
CREATE TABLE embeddings (
	sha256 TEXT NOT NULL,
	model  TEXT NOT NULL,
	vec    BLOB NOT NULL,
	PRIMARY KEY (sha256, model)
);
`,
	// 14: faces, found by trunk-ai per file content, and the people they are
	// grouped into, per user. A box is relative to the picture (0..1).
	// face_people: manual is a choice the user made (kept by regrouping),
	// rejected a person the face must not go to again.
	`
CREATE TABLE faces (
	id      INTEGER PRIMARY KEY,
	sha256  TEXT NOT NULL,
	model   TEXT NOT NULL,
	x       REAL NOT NULL, y REAL NOT NULL, w REAL NOT NULL, h REAL NOT NULL,
	score   REAL NOT NULL,
	frontal INTEGER NOT NULL,
	vec     BLOB NOT NULL
);
CREATE INDEX faces_sha ON faces (sha256, model);
CREATE TABLE face_scans (
	sha256 TEXT NOT NULL,
	model  TEXT NOT NULL,
	PRIMARY KEY (sha256, model)
);
CREATE TABLE people (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name       TEXT NOT NULL DEFAULT '',
	hidden     INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE face_people (
	user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	face_id   INTEGER NOT NULL REFERENCES faces(id) ON DELETE CASCADE,
	person_id INTEGER REFERENCES people(id) ON DELETE SET NULL,
	manual    INTEGER NOT NULL DEFAULT 0,
	rejected  INTEGER,
	PRIMARY KEY (user_id, face_id)
);
CREATE INDEX face_people_person ON face_people (person_id);
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
