package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/carnager/trunk/internal/db"
)

// Album is an ordered list of the user's assets. Like assets it carries a seq
// and travels in the changes feed, always with its complete list, so clients
// simply replace their copy. A deleted album is a tombstone (ID, Seq, Deleted).
type Album struct {
	ID        int64      `json:"id"`
	Seq       int64      `json:"seq"`
	Deleted   bool       `json:"deleted,omitempty"`
	Name      string     `json:"name,omitempty"`
	AssetIDs  []int64    `json:"asset_ids"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`

	userID int64
}

const albumCols = `id, user_id, seq, name, created_at, updated_at, deleted_at`

func (s *Server) scanAlbum(ctx context.Context, sc scanner) (*Album, error) {
	var a Album
	var created, updated int64
	var deleted sql.NullInt64
	if err := sc.Scan(&a.ID, &a.userID, &a.Seq, &a.Name, &created, &updated, &deleted); err != nil {
		return nil, err
	}
	if deleted.Valid {
		return &Album{ID: a.ID, Seq: a.Seq, Deleted: true, AssetIDs: nil, userID: a.userID}, nil
	}
	c, u := time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	a.CreatedAt, a.UpdatedAt = &c, &u
	return &a, nil
}

// fillAlbumAssets loads the asset lists of live albums in one query.
func (s *Server) fillAlbumAssets(ctx context.Context, albums []*Album) error {
	byID := map[int64]*Album{}
	var ids []any
	for _, a := range albums {
		if !a.Deleted {
			a.AssetIDs = []int64{}
			byID[a.ID] = a
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT album_id, asset_id FROM album_assets
		WHERE album_id IN (`+placeholders(len(ids))+`) ORDER BY album_id, pos`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var al, as int64
		if err := rows.Scan(&al, &as); err != nil {
			return err
		}
		byID[al].AssetIDs = append(byID[al].AssetIDs, as)
	}
	return rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (s *Server) getAlbum(ctx context.Context, userID, id int64) (*Album, error) {
	a, err := s.scanAlbum(ctx, s.db.QueryRowContext(ctx,
		`SELECT `+albumCols+` FROM albums WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && a.Deleted) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, s.fillAlbumAssets(ctx, []*Album{a})
}

func (s *Server) albumFromPath(w http.ResponseWriter, r *http.Request) (*Album, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	a, err := s.getAlbum(r.Context(), sessionFrom(r).UserID, id)
	if errors.Is(err, errNotFound) {
		httpError(w, http.StatusNotFound, "no such album")
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return a, true
}

// albumChanges returns albums changed after since, oldest change first.
func (s *Server) albumChanges(ctx context.Context, userID, since int64, limit int) ([]*Album, error) {
	q := `SELECT ` + albumCols + ` FROM albums WHERE user_id = ? AND seq > ?`
	if since == 0 {
		q += ` AND deleted_at IS NULL`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY seq LIMIT ?`, userID, since, limit)
	if err != nil {
		return nil, err
	}
	var list []*Album
	for rows.Next() {
		a, err := s.scanAlbum(ctx, rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, s.fillAlbumAssets(ctx, list)
}

// inTx runs fn in a transaction with a fresh seq for the change.
func (s *Server) inTx(ctx context.Context, fn func(tx *sql.Tx, seq, now int64) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := db.NextSeq(ctx, tx)
	if err != nil {
		return err
	}
	if err := fn(tx, seq, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

var errNotYours = errors.New("not your asset")

// CreateAlbum makes a new, empty album.
func (s *Server) CreateAlbum(ctx context.Context, userID int64, name string) (int64, error) {
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx, seq, now int64) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO albums (user_id, seq, name, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, userID, seq, name, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// AlbumByName finds a live album of the user by exact name.
func (s *Server) AlbumByName(ctx context.Context, userID int64, name string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM albums WHERE user_id = ? AND name = ? AND deleted_at IS NULL
		ORDER BY id LIMIT 1`, userID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// AddToAlbum puts assets into an album, at the front or the end, in the order
// given. Assets already in it keep their place. All must belong to the user.
func (s *Server) AddToAlbum(ctx context.Context, userID, albumID int64, ids []int64, atEnd bool) error {
	return s.inTx(ctx, func(tx *sql.Tx, seq, now int64) error {
		for _, id := range ids {
			var owner int64
			err := tx.QueryRowContext(ctx, `SELECT user_id FROM assets WHERE id = ? AND deleted_at IS NULL`, id).Scan(&owner)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != userID) {
				return errNotYours
			} else if err != nil {
				return err
			}
		}
		var lo, hi sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(pos), MAX(pos) FROM album_assets WHERE album_id = ?`,
			albumID).Scan(&lo, &hi); err != nil {
			return err
		}
		for i, id := range ids {
			// Front: the first id given ends up first.
			pos := lo.Int64 - int64(len(ids)) + int64(i)
			if atEnd {
				pos = hi.Int64 + 1 + int64(i)
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO album_assets (album_id, asset_id, pos, added_at)
				VALUES (?, ?, ?, ?)`, albumID, id, pos, now); err != nil {
				return err
			}
		}
		return touchAlbum(ctx, tx, albumID, seq, now)
	})
}

func touchAlbum(ctx context.Context, tx *sql.Tx, albumID, seq, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE albums SET seq = ?, updated_at = ? WHERE id = ?`, seq, now, albumID)
	return err
}

// unlistAsset drops a deleted asset from every album, so their lists in the
// feed never point at tombstones. Each album gets a seq of its own: rows that
// shared one could be split across feed pages and the rest skipped.
func unlistAsset(ctx context.Context, tx *sql.Tx, assetID, now int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT album_id FROM album_assets WHERE asset_id = ?`, assetID)
	if err != nil {
		return err
	}
	var albums []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			albums = append(albums, id)
		}
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM album_assets WHERE asset_id = ?`, assetID); err != nil {
		return err
	}
	for _, id := range albums {
		seq, err := db.NextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if err := touchAlbum(ctx, tx, id, seq, now); err != nil {
			return err
		}
	}
	return nil
}

// --- handlers ---

func (s *Server) handleCreateAlbum(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	var req struct {
		Name     string  `json:"name"`
		AssetIDs []int64 `json:"asset_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, ok := albumName(w, req.Name)
	if !ok {
		return
	}
	id, err := s.CreateAlbum(r.Context(), sess.UserID, name)
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(req.AssetIDs) > 0 {
		if err := s.AddToAlbum(r.Context(), sess.UserID, id, req.AssetIDs, true); err != nil {
			s.albumError(w, err)
			return
		}
	}
	a, err := s.getAlbum(r.Context(), sess.UserID, id)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func albumName(w http.ResponseWriter, n string) (string, bool) {
	n = strings.TrimSpace(n)
	if n == "" || len(n) > 200 {
		httpError(w, http.StatusBadRequest, "album name must be 1 to 200 characters")
		return "", false
	}
	return n, true
}

func (s *Server) albumError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotYours) {
		httpError(w, http.StatusBadRequest, "albums can only hold your own live assets")
		return
	}
	s.internal(w, err)
}

func (s *Server) handleGetAlbum(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.albumFromPath(w, r); ok {
		writeJSON(w, http.StatusOK, a)
	}
}

func (s *Server) handlePatchAlbum(w http.ResponseWriter, r *http.Request) {
	a, ok := s.albumFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Name *string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name, ok := albumName(w, *req.Name)
		if !ok {
			return
		}
		err := s.inTx(r.Context(), func(tx *sql.Tx, seq, now int64) error {
			_, err := tx.ExecContext(r.Context(), `UPDATE albums SET name = ?, seq = ?, updated_at = ? WHERE id = ?`,
				name, seq, now, a.ID)
			return err
		})
		if err != nil {
			s.internal(w, err)
			return
		}
	}
	s.respondAlbum(w, r, a)
}

func (s *Server) respondAlbum(w http.ResponseWriter, r *http.Request, a *Album) {
	fresh, err := s.getAlbum(r.Context(), a.userID, a.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

func (s *Server) handleAlbumAdd(w http.ResponseWriter, r *http.Request) {
	a, ok := s.albumFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		IDs []int64 `json:"ids"`
		End bool    `json:"end"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 5000 {
		httpError(w, http.StatusBadRequest, "need 1 to 5000 ids")
		return
	}
	if err := s.AddToAlbum(r.Context(), a.userID, a.ID, req.IDs, req.End); err != nil {
		s.albumError(w, err)
		return
	}
	s.respondAlbum(w, r, a)
}

func (s *Server) handleAlbumRemove(w http.ResponseWriter, r *http.Request) {
	a, ok := s.albumFromPath(w, r)
	if !ok {
		return
	}
	ids, ok := readIDs(w, r)
	if !ok {
		return
	}
	err := s.inTx(r.Context(), func(tx *sql.Tx, seq, now int64) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM album_assets WHERE album_id = ? AND asset_id = ?`,
				a.ID, id); err != nil {
				return err
			}
		}
		return touchAlbum(r.Context(), tx, a.ID, seq, now)
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	s.respondAlbum(w, r, a)
}

// handleDeleteAlbum removes the album; its photos stay in the library.
func (s *Server) handleDeleteAlbum(w http.ResponseWriter, r *http.Request) {
	a, ok := s.albumFromPath(w, r)
	if !ok {
		return
	}
	err := s.inTx(r.Context(), func(tx *sql.Tx, seq, now int64) error {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM album_assets WHERE album_id = ?`, a.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `UPDATE albums SET deleted_at = ?, seq = ?, updated_at = ? WHERE id = ?`,
			now, seq, now, a.ID)
		return err
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
