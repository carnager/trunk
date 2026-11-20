package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/carnager/trunk/internal/db"
)

// Album is an ordered list of assets. Like assets it carries a seq and
// travels in the changes feed, always with its complete list, so clients
// simply replace their copy. A deleted album, or one the caller no longer
// shares, is a tombstone (ID, Seq, Deleted).
//
// Albums can be shared within the household: members see them and, if
// allowed, add their own photos. Members' feeds carry the album but not its
// assets, which are not theirs; GET /albums/{id}/assets returns them, and
// any change to an asset in a shared album gives the album a new seq.
type Album struct {
	ID        int64      `json:"id"`
	Seq       int64      `json:"seq"`
	Deleted   bool       `json:"deleted,omitempty"`
	Name      string     `json:"name,omitempty"`
	AssetIDs  []int64    `json:"asset_ids"`
	Owner     *UserRef   `json:"owner,omitempty"`
	Members   []Member   `json:"members,omitempty"`
	Kind      string     `json:"kind,omitempty"`    // "direct" for photos sent to someone; empty for albums
	Role      string     `json:"role,omitempty"`    // owner | member
	CanAdd    bool       `json:"can_add,omitempty"` // whether the caller may add photos
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`

	userID int64
}

// UserRef names an account.
type UserRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Member is someone an album is shared with.
type Member struct {
	UserRef
	CanAdd bool `json:"can_add"`
}

// albumSel selects albums as one user (the first two arguments) sees them:
// their own and those shared with them. The seq is the later of the album's
// and the membership's, so joining or leaving reaches the member's feed; a
// membership that ended keeps the seq of its end, and the album is gone.
const albumSel = `SELECT al.id, al.user_id, ow.name,
	CASE WHEN m.removed_at IS NOT NULL THEN m.seq ELSE MAX(al.seq, COALESCE(m.seq, 0)) END AS eseq,
	al.name, al.kind, al.created_at, al.updated_at,
	(al.deleted_at IS NOT NULL OR m.removed_at IS NOT NULL) AS gone,
	COALESCE(m.can_add, 1)
	FROM albums al JOIN users ow ON ow.id = al.user_id
	LEFT JOIN album_members m ON m.album_id = al.id AND m.user_id = ?
	WHERE (al.user_id = ? OR m.user_id IS NOT NULL)`

func (s *Server) scanAlbum(sc scanner, viewer int64) (*Album, error) {
	var a Album
	var ownerName string
	var created, updated int64
	var gone, canAdd bool
	var kind string
	if err := sc.Scan(&a.ID, &a.userID, &ownerName, &a.Seq, &a.Name, &kind, &created, &updated, &gone, &canAdd); err != nil {
		return nil, err
	}
	if kind != "album" {
		a.Kind = kind
	}
	if gone {
		return &Album{ID: a.ID, Seq: a.Seq, Deleted: true, userID: a.userID}, nil
	}
	if a.userID == viewer {
		a.Role, a.CanAdd = "owner", true
	} else {
		a.Role, a.CanAdd = "member", canAdd
		a.Owner = &UserRef{a.userID, ownerName}
	}
	c, u := time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	a.CreatedAt, a.UpdatedAt = &c, &u
	return &a, nil
}

// fillAlbums loads the asset lists and members of live albums.
func (s *Server) fillAlbums(ctx context.Context, albums []*Album) error {
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
	for rows.Next() {
		var al, as int64
		if err := rows.Scan(&al, &as); err != nil {
			rows.Close()
			return err
		}
		byID[al].AssetIDs = append(byID[al].AssetIDs, as)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT m.album_id, u.id, u.name, m.can_add FROM album_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.album_id IN (`+placeholders(len(ids))+`) AND m.removed_at IS NULL ORDER BY u.name`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var al int64
		var m Member
		if err := rows.Scan(&al, &m.ID, &m.Name, &m.CanAdd); err != nil {
			return err
		}
		byID[al].Members = append(byID[al].Members, m)
	}
	return rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// getAlbum loads an album the user owns or shares.
func (s *Server) getAlbum(ctx context.Context, userID, id int64) (*Album, error) {
	a, err := s.scanAlbum(s.db.QueryRowContext(ctx, albumSel+` AND al.id = ?`, userID, userID, id), userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && a.Deleted) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, s.fillAlbums(ctx, []*Album{a})
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

// ownAlbumFromPath is albumFromPath for changes only the owner may make.
func (s *Server) ownAlbumFromPath(w http.ResponseWriter, r *http.Request) (*Album, bool) {
	a, ok := s.albumFromPath(w, r)
	if ok && a.Role != "owner" {
		httpError(w, http.StatusForbidden, "only the album's owner can do that")
		return nil, false
	}
	return a, ok
}

// albumChanges returns albums changed after since, oldest change first.
func (s *Server) albumChanges(ctx context.Context, userID, since int64, limit int) ([]*Album, error) {
	q := albumSel + ` AND eseq > ?`
	if since == 0 {
		q += ` AND NOT gone`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY eseq LIMIT ?`, userID, userID, since, limit)
	if err != nil {
		return nil, err
	}
	var list []*Album
	for rows.Next() {
		a, err := s.scanAlbum(rows, userID)
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
	return list, s.fillAlbums(ctx, list)
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
	err := s.db.QueryRowContext(ctx, `SELECT id FROM albums WHERE user_id = ? AND name = ? AND kind = 'album'
		AND deleted_at IS NULL ORDER BY id LIMIT 1`, userID, name).Scan(&id)
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
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO album_assets (album_id, asset_id, pos, added_at, added_by)
				VALUES (?, ?, ?, ?, ?)`, albumID, id, pos, now, userID); err != nil {
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

// touchSharedAlbums gives every shared album that holds one of the assets a
// new seq: its members do not get those assets in their feed, so this is how
// they learn to fetch the album's assets again after an edit, a new caption
// or a trip to the trash.
func touchSharedAlbums(ctx context.Context, tx *sql.Tx, assetIDs []int64, now int64) error {
	if len(assetIDs) == 0 {
		return nil
	}
	args := make([]any, len(assetIDs))
	for i, id := range assetIDs {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT aa.album_id FROM album_assets aa
		WHERE aa.asset_id IN (`+placeholders(len(args))+`)
		AND EXISTS (SELECT 1 FROM album_members m WHERE m.album_id = aa.album_id AND m.removed_at IS NULL)`, args...)
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
	a, ok := s.ownAlbumFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Name *string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name != nil && a.Kind == "direct" {
		httpError(w, http.StatusBadRequest, "photos sent to someone have no name of their own")
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

// respondAlbum answers with the album as the caller sees it now.
func (s *Server) respondAlbum(w http.ResponseWriter, r *http.Request, a *Album) {
	fresh, err := s.getAlbum(r.Context(), sessionFrom(r).UserID, a.ID)
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
	if !a.CanAdd {
		httpError(w, http.StatusForbidden, "you may look at this album but not add to it")
		return
	}
	// Everyone adds their own photos, also to an album someone else owns.
	if err := s.AddToAlbum(r.Context(), sessionFrom(r).UserID, a.ID, req.IDs, req.End); err != nil {
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
	// The owner takes out anything; a member only what they added.
	me := sessionFrom(r).UserID
	err := s.inTx(r.Context(), func(tx *sql.Tx, seq, now int64) error {
		for _, id := range ids {
			q, args := `DELETE FROM album_assets WHERE album_id = ? AND asset_id = ?`, []any{a.ID, id}
			if a.Role != "owner" {
				q, args = q+` AND added_by = ?`, append(args, me)
			}
			if _, err := tx.ExecContext(r.Context(), q, args...); err != nil {
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
	a, ok := s.ownAlbumFromPath(w, r)
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

// handleAlbumAssets returns an album's assets in full, also those of other
// people in a shared album, which are not in the caller's feed.
func (s *Server) handleAlbumAssets(w http.ResponseWriter, r *http.Request) {
	a, ok := s.albumFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	me := sessionFrom(r).UserID
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed("a.", assetCols)+`, u.name FROM album_assets aa
		JOIN assets a ON a.id = aa.asset_id AND a.deleted_at IS NULL
		JOIN users u ON u.id = a.user_id
		WHERE aa.album_id = ? ORDER BY aa.pos`, a.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	list := []*Asset{}
	for rows.Next() {
		var owner string
		as, err := s.scanAsset(scanWith(rows, &owner))
		if err != nil {
			s.internal(w, err)
			return
		}
		if as.userID != me {
			as.Owner = owner
		}
		localize(ctx, as)
		list = append(list, as)
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": list})
}

// scanWith appends extra destinations to the ones scanAsset passes.
type extraScanner struct {
	sc    scanner
	extra []any
}

func (e extraScanner) Scan(dest ...any) error { return e.sc.Scan(append(dest, e.extra...)...) }

func scanWith(sc scanner, extra ...any) scanner { return extraScanner{sc, extra} }

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// handleSetMember shares the album with a household member, or changes
// whether they may add photos: PUT /albums/{id}/members/{user} {"can_add"}.
func (s *Server) handleSetMember(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ownAlbumFromPath(w, r)
	if !ok {
		return
	}
	if a.Kind == "direct" {
		httpError(w, http.StatusBadRequest, "send photos to someone else with POST /shares")
		return
	}
	userID, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil || userID == a.userID {
		httpError(w, http.StatusBadRequest, "bad user")
		return
	}
	var req struct {
		CanAdd *bool `json:"can_add"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	canAdd := req.CanAdd == nil || *req.CanAdd
	var exists int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&exists); err != nil || exists == 0 {
		httpError(w, http.StatusNotFound, "no such user")
		return
	}
	err = s.inTx(r.Context(), func(tx *sql.Tx, seq, now int64) error {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO album_members (album_id, user_id, can_add, seq) VALUES (?, ?, ?, ?)
			ON CONFLICT (album_id, user_id) DO UPDATE SET can_add = excluded.can_add, seq = excluded.seq, removed_at = NULL`,
			a.ID, userID, boolInt(canAdd), seq); err != nil {
			return err
		}
		// The owner's other devices learn about the member through the album.
		albumSeq, err := db.NextSeq(r.Context(), tx)
		if err != nil {
			return err
		}
		return touchAlbum(r.Context(), tx, a.ID, albumSeq, now)
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	s.respondAlbum(w, r, a)
}

// handleRemoveMember stops sharing with someone; members may remove
// themselves, i.e. leave. Photos they added stay in the album.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	a, ok := s.albumFromPath(w, r)
	if !ok {
		return
	}
	me := sessionFrom(r).UserID
	userID, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad user")
		return
	}
	if a.Role != "owner" && userID != me {
		httpError(w, http.StatusForbidden, "only the album's owner can do that")
		return
	}
	err = s.inTx(r.Context(), func(tx *sql.Tx, seq, now int64) error {
		res, err := tx.ExecContext(r.Context(), `UPDATE album_members SET removed_at = ?, seq = ?
			WHERE album_id = ? AND user_id = ? AND removed_at IS NULL`, now, seq, a.ID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNotFound
		}
		albumSeq, err := db.NextSeq(r.Context(), tx)
		if err != nil {
			return err
		}
		return touchAlbum(r.Context(), tx, a.ID, albumSeq, now)
	})
	if errors.Is(err, errNotFound) {
		httpError(w, http.StatusNotFound, "not a member")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUsers lists the household: the other accounts on this server, to
// share albums with.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, name FROM users WHERE id != ? ORDER BY name`, sessionFrom(r).UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	list := []UserRef{}
	for rows.Next() {
		var u UserRef
		if rows.Scan(&u.ID, &u.Name) == nil {
			list = append(list, u)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": list})
}

// handleShare sends photos to someone in the household:
//
//	POST /api/v1/shares {"user_id": 2, "asset_ids": [..]}
//
// They go to the front of the one "direct" album from the caller to that
// person, made on first use and shared with them view-only. The answer is
// that album. Taking a photo back is removing it from the album; deleting
// the album ends the sharing; the recipient can leave it to hide it.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := sessionFrom(r).UserID
	var req struct {
		UserID   int64   `json:"user_id"`
		AssetIDs []int64 `json:"asset_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.AssetIDs) == 0 || len(req.AssetIDs) > 5000 {
		httpError(w, http.StatusBadRequest, "need 1 to 5000 asset_ids")
		return
	}
	var name string
	if req.UserID == me || s.db.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?`, req.UserID).Scan(&name) != nil {
		httpError(w, http.StatusBadRequest, "no such person")
		return
	}
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	var albumID int64
	err := s.db.QueryRowContext(ctx, `SELECT al.id FROM albums al JOIN album_members m ON m.album_id = al.id
		WHERE al.user_id = ? AND al.kind = 'direct' AND al.deleted_at IS NULL AND m.user_id = ?
		ORDER BY al.id LIMIT 1`, me, req.UserID).Scan(&albumID)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.inTx(ctx, func(tx *sql.Tx, seq, now int64) error {
			res, err := tx.ExecContext(ctx, `INSERT INTO albums (user_id, seq, name, kind, created_at, updated_at)
				VALUES (?, ?, ?, 'direct', ?, ?)`, me, seq, name, now, now)
			if err != nil {
				return err
			}
			if albumID, err = res.LastInsertId(); err != nil {
				return err
			}
			memberSeq, err := db.NextSeq(ctx, tx)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO album_members (album_id, user_id, can_add, seq) VALUES (?, ?, 0, ?)`,
				albumID, req.UserID, memberSeq)
			return err
		})
	} else if err == nil {
		// Hidden by the recipient earlier: sending again brings it back.
		err = s.inTx(ctx, func(tx *sql.Tx, seq, now int64) error {
			_, err := tx.ExecContext(ctx, `UPDATE album_members SET removed_at = NULL, seq = ?
				WHERE album_id = ? AND user_id = ? AND removed_at IS NOT NULL`, seq, albumID, req.UserID)
			return err
		})
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if err := s.AddToAlbum(ctx, me, albumID, req.AssetIDs, false); err != nil {
		s.albumError(w, err)
		return
	}
	a, err := s.getAlbum(ctx, me, albumID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
