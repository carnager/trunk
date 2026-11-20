package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A library created by the first release keeps its assets findable by hash.
func TestMigrateFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunk.db")
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		migrations[0],
		`PRAGMA user_version = 1`,
		`INSERT INTO users (id, name, pw_hash, created_at) VALUES (1, 'alice', 'x', 0)`,
		`INSERT INTO assets (user_id, seq, sha256, path, orig_name, kind, mime, size, taken_at, created_at, updated_at)
			VALUES (1, 1, 'aa', 'alice/a.jpg', 'a.jpg', 'image', 'image/jpeg', 1, 0, 0, 0),
			       (1, 2, 'bb', '', 'b.jpg', 'image', 'image/jpeg', 1, 0, 0, 0)`,
		`UPDATE assets SET deleted_at = 5 WHERE sha256 = 'bb'`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v, n int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	d.QueryRow(`SELECT COUNT(*) FROM asset_hashes WHERE sha256 = 'aa' AND kind = 'original'`).Scan(&n)
	if v != len(migrations) || n != 1 {
		t.Errorf("version %d, live hash rows %d", v, n)
	}
	d.QueryRow(`SELECT COUNT(*) FROM asset_hashes WHERE sha256 = 'bb'`).Scan(&n)
	if n != 0 {
		t.Error("tombstone got a hash row")
	}
}
