package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/carnager/trunk/internal/db"
	"github.com/carnager/trunk/internal/places"
	"golang.org/x/text/language"
)

// Asset is one photo or video as the API presents it. A deleted asset is sent
// as a tombstone with only ID, Seq and Deleted set.
type Asset struct {
	ID      int64  `json:"id"`
	Seq     int64  `json:"seq"`
	Deleted bool   `json:"deleted,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind,omitempty"`
	MIME    string `json:"mime,omitempty"`
	Size    int64  `json:"size,omitempty"`

	TakenAt   *time.Time `json:"taken_at,omitempty"`   // UTC instant
	TZOffset  *int       `json:"tz_offset,omitempty"`  // minutes east of UTC, absent if unknown
	LocalTime string     `json:"local_time,omitempty"` // wall clock where it was taken

	Width       int      `json:"width,omitempty"` // display size, orientation applied
	Height      int      `json:"height,omitempty"`
	Orientation int      `json:"orientation,omitempty"`
	DurationMS  int64    `json:"duration_ms,omitempty"`
	Lat         *float64 `json:"lat,omitempty"`
	Lon         *float64 `json:"lon,omitempty"`
	Alt         *float64 `json:"alt,omitempty"`
	Make        string   `json:"make,omitempty"`
	Model       string   `json:"model,omitempty"`
	Lens        string   `json:"lens,omitempty"`
	FNumber     float64  `json:"f_number,omitempty"`
	Exposure    float64  `json:"exposure,omitempty"` // seconds
	ISO         int      `json:"iso,omitempty"`
	FocalLength float64  `json:"focal_length,omitempty"`
	// The nearest town to Lat/Lon, in the language the client asked for.
	Place   *places.Names `json:"place,omitempty"`
	Caption string        `json:"caption,omitempty"`
	// Set only for someone else's photo, seen through a shared album.
	Owner string `json:"owner,omitempty"`

	// Set once the asset has been edited: the current file differs from the
	// first upload, which /original still returns.
	EditedAt       *time.Time `json:"edited_at,omitempty"`
	OriginalSHA256 string     `json:"original_sha256,omitempty"`
	OriginalSize   int64      `json:"original_size,omitempty"`
	OriginalMIME   string     `json:"original_mime,omitempty"`

	Favorite       bool       `json:"favorite"`
	TrashedAt      *time.Time `json:"trashed_at,omitempty"`
	TrashExpiresAt *time.Time `json:"trash_expires_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`

	userID   int64
	placeID  int64
	path     string // current file, relative to the originals dir
	origPath string // first upload, if edited
}

const assetCols = `id, user_id, seq, sha256, path, orig_name, kind, mime, size, taken_at, tz_offset,
	width, height, orientation, duration_ms, lat, lon, alt, make, model, lens, f_number, exposure, iso,
	focal_length, favorite, trashed_at, deleted_at, created_at, updated_at,
	orig_path, orig_sha256, orig_size, orig_mime, edited_at, place_id, caption`

type scanner interface{ Scan(...any) error }

func (s *Server) scanAsset(sc scanner) (*Asset, error) {
	var a Asset
	var (
		taken, created, updated          int64
		tz, w, h, orient, dur, iso       sql.NullInt64
		lat, lon, alt, fnum, expo, focal sql.NullFloat64
		mk, model, lens                  sql.NullString
		fav                              int
		trashed, deleted                 sql.NullInt64
		oPath, oSHA, oMIME               sql.NullString
		oSize, edited                    sql.NullInt64
		placeID                          sql.NullInt64
		caption                          sql.NullString
	)
	err := sc.Scan(&a.ID, &a.userID, &a.Seq, &a.SHA256, &a.path, &a.Name, &a.Kind, &a.MIME, &a.Size,
		&taken, &tz, &w, &h, &orient, &dur, &lat, &lon, &alt, &mk, &model, &lens, &fnum, &expo, &iso,
		&focal, &fav, &trashed, &deleted, &created, &updated,
		&oPath, &oSHA, &oSize, &oMIME, &edited, &placeID, &caption)
	if err != nil {
		return nil, err
	}
	if deleted.Valid {
		return &Asset{ID: a.ID, Seq: a.Seq, Deleted: true, userID: a.userID}, nil
	}
	t := time.UnixMilli(taken).UTC()
	a.TakenAt = &t
	local := t
	if tz.Valid {
		o := int(tz.Int64)
		a.TZOffset = &o
		local = t.Add(time.Duration(o) * time.Minute)
	}
	a.LocalTime = local.Format("2006-01-02T15:04:05")
	a.Width, a.Height, a.Orientation = int(w.Int64), int(h.Int64), int(orient.Int64)
	a.DurationMS, a.ISO = dur.Int64, int(iso.Int64)
	a.Lat, a.Lon, a.Alt = nullF(lat), nullF(lon), nullF(alt)
	a.Make, a.Model, a.Lens = mk.String, model.String, lens.String
	a.FNumber, a.Exposure, a.FocalLength = fnum.Float64, expo.Float64, focal.Float64
	a.Favorite = fav == 1
	if trashed.Valid {
		tt := time.UnixMilli(trashed.Int64).UTC()
		exp := tt.AddDate(0, 0, s.cfg.TrashDays)
		a.TrashedAt, a.TrashExpiresAt = &tt, &exp
	}
	a.placeID, a.Caption = placeID.Int64, caption.String
	if edited.Valid {
		e := time.UnixMilli(edited.Int64).UTC()
		a.EditedAt = &e
		a.origPath, a.OriginalSHA256, a.OriginalSize, a.OriginalMIME = oPath.String, oSHA.String, oSize.Int64, oMIME.String
	}
	c, u := time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	a.CreatedAt, a.UpdatedAt = &c, &u
	return &a, nil
}

func nullF(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

var errNotFound = errors.New("not found")

// getAsset loads a live (not deleted) asset owned by userID.
func (s *Server) getAsset(ctx context.Context, userID, id int64) (*Asset, error) {
	a, err := s.scanAsset(s.db.QueryRowContext(ctx,
		`SELECT `+assetCols+` FROM assets WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && a.Deleted) {
		return nil, errNotFound
	}
	if err == nil {
		localize(ctx, a)
	}
	return a, err
}

type langKey struct{}

// withLanguage remembers the request's preferred languages for place names.
func withLanguage(ctx context.Context, r *http.Request) context.Context {
	tags, _, _ := language.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
	return context.WithValue(ctx, langKey{}, tags)
}

// localize fills in the place names in the caller's language.
func localize(ctx context.Context, a *Asset) {
	if a.placeID <= 0 {
		return
	}
	if p := places.ByID(a.placeID); p != nil {
		tags, _ := ctx.Value(langKey{}).([]language.Tag)
		n := p.In(tags...)
		a.Place = &n
	}
}

// visibleAsset loads an asset the user owns or sees through a shared album.
func (s *Server) visibleAsset(ctx context.Context, userID, id int64) (*Asset, error) {
	var owner string
	a, err := s.scanAsset(scanWith(s.db.QueryRowContext(ctx, `SELECT `+prefixed("a.", assetCols)+`, u.name
		FROM assets a JOIN users u ON u.id = a.user_id WHERE a.id = ?`, id), &owner))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && a.Deleted) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	if a.userID != userID {
		var n int
		// In an album the user owns, or one shared with them.
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_assets aa
			JOIN albums al ON al.id = aa.album_id AND al.deleted_at IS NULL
			LEFT JOIN album_members m ON m.album_id = al.id AND m.user_id = ? AND m.removed_at IS NULL
			WHERE aa.asset_id = ? AND (al.user_id = ? OR m.user_id IS NOT NULL)`, userID, id, userID).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, errNotFound
		}
		a.Owner = owner
	}
	localize(ctx, a)
	return a, nil
}

// readableAssetFromPath is assetFromPath for looking: shared photos count.
func (s *Server) readableAssetFromPath(w http.ResponseWriter, r *http.Request) (*Asset, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	a, err := s.visibleAsset(r.Context(), sessionFrom(r).UserID, id)
	if errors.Is(err, errNotFound) {
		httpError(w, http.StatusNotFound, "no such asset")
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return a, true
}

// assetFromPath loads an asset of the caller's own, for changing it.
func (s *Server) assetFromPath(w http.ResponseWriter, r *http.Request) (*Asset, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	a, err := s.getAsset(r.Context(), sessionFrom(r).UserID, id)
	if errors.Is(err, errNotFound) {
		httpError(w, http.StatusNotFound, "no such asset")
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return a, true
}

// handleChanges is the delta sync feed: every asset and album whose seq is
// greater than `since`, oldest change first. Clients store `cursor` and pass
// it as `since` next time; while `more` is true there is another page waiting.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	ctx := r.Context()
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	// Read the global seq before querying: any change that commits later gets
	// a larger seq, so handing this out as the cursor can never skip one.
	var head int64
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'seq'`).Scan(&head); err != nil {
		s.internal(w, err)
		return
	}
	q := `SELECT ` + assetCols + ` FROM assets WHERE user_id = ? AND seq > ?`
	if since == 0 {
		q += ` AND deleted_at IS NULL` // a fresh client has nothing to delete
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY seq LIMIT ?`, sess.UserID, since, limit+1)
	if err != nil {
		s.internal(w, err)
		return
	}
	assets := []*Asset{}
	for rows.Next() {
		a, err := s.scanAsset(rows)
		if err != nil {
			rows.Close()
			s.internal(w, err)
			return
		}
		localize(ctx, a)
		assets = append(assets, a)
	}
	rows.Close()
	albums, err := s.albumChanges(ctx, sess.UserID, since, limit+1)
	if err != nil {
		s.internal(w, err)
		return
	}

	// Merge both by seq and keep the first `limit` changes.
	outAssets, outAlbums := []*Asset{}, []*Album{}
	i, j := 0, 0
	cursor := since
	for len(outAssets)+len(outAlbums) < limit && (i < len(assets) || j < len(albums)) {
		if j >= len(albums) || (i < len(assets) && assets[i].Seq < albums[j].Seq) {
			outAssets = append(outAssets, assets[i])
			cursor = assets[i].Seq
			i++
		} else {
			outAlbums = append(outAlbums, albums[j])
			cursor = albums[j].Seq
			j++
		}
	}
	more := i < len(assets) || j < len(albums)
	if !more && head > cursor {
		cursor = head // skip past other users' changes
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": outAssets, "albums": outAlbums, "cursor": cursor, "more": more})
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// handleCheck tells a client which of its files the server already has, so it
// can skip uploading them. Every hash an asset is known by counts: the first
// upload, the client's source before shrinking, the current edit. Trashed
// assets count as present.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	var req struct {
		SHA256 []string `json:"sha256"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.SHA256) > 5000 {
		httpError(w, http.StatusBadRequest, "at most 5000 hashes per request")
		return
	}
	found := map[string]int64{}
	for _, h := range req.SHA256 {
		h = strings.ToLower(h)
		if !reSHA.MatchString(h) {
			httpError(w, http.StatusBadRequest, "bad sha256: "+h)
			return
		}
		id, ok, err := s.findBySHA(r.Context(), sess.UserID, h)
		if err != nil {
			s.internal(w, err)
			return
		}
		if ok {
			found[h] = id
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"existing": found})
}

func (s *Server) handleGetAsset(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.readableAssetFromPath(w, r); ok {
		writeJSON(w, http.StatusOK, a)
	}
}

func (s *Server) handlePatchAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := s.assetFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Favorite *bool   `json:"favorite"`
		Caption  *string `json:"caption"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	changed := false
	if req.Favorite != nil && *req.Favorite != a.Favorite {
		if err := s.mutate(r.Context(), a.userID, []int64{a.ID}, `favorite = ?`, boolInt(*req.Favorite)); err != nil {
			s.internal(w, err)
			return
		}
		changed = true
	}
	if req.Caption != nil {
		c := strings.TrimSpace(*req.Caption)
		if len(c) > 4000 {
			httpError(w, http.StatusBadRequest, "caption too long")
			return
		}
		if c != a.Caption {
			if err := s.mutate(r.Context(), a.userID, []int64{a.ID}, `caption = ?`, ns(c)); err != nil {
				s.internal(w, err)
				return
			}
			changed = true
		}
	}
	if changed {
		a, _ = s.getAsset(r.Context(), a.userID, a.ID)
	}
	writeJSON(w, http.StatusOK, a)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mutate applies `set` to the given live assets of userID, stamping each with
// a new seq so the change reaches every device.
func (s *Server) mutate(ctx context.Context, userID int64, ids []int64, set string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, id := range ids {
		seq, err := db.NextSeq(ctx, tx)
		if err != nil {
			return err
		}
		a := append(append([]any{}, args...), seq, now, id, userID)
		if _, err := tx.ExecContext(ctx, `UPDATE assets SET `+set+`, seq = ?, updated_at = ?
			WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, a...); err != nil {
			return err
		}
	}
	if err := touchSharedAlbums(ctx, tx, ids, now); err != nil {
		return err
	}
	return tx.Commit()
}

func readIDs(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &req) {
		return nil, false
	}
	if len(req.IDs) == 0 || len(req.IDs) > 5000 {
		httpError(w, http.StatusBadRequest, "need 1 to 5000 ids")
		return nil, false
	}
	return req.IDs, true
}

func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	ids, ok := readIDs(w, r)
	if !ok {
		return
	}
	err := s.mutate(r.Context(), sessionFrom(r).UserID, ids, `trashed_at = COALESCE(trashed_at, ?)`, time.Now().UnixMilli())
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	ids, ok := readIDs(w, r)
	if !ok {
		return
	}
	if err := s.mutate(r.Context(), sessionFrom(r).UserID, ids, `trashed_at = NULL`); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDelete removes assets for good. Only trashed assets can be deleted,
// so one stray request cannot wipe photos without passing through the trash.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ids, ok := readIDs(w, r)
	if !ok {
		return
	}
	n, err := s.purge(r.Context(), sessionFrom(r).UserID, ids)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// purge deletes trashed assets: the row becomes a tombstone and the original
// and its thumbnails are removed from disk.
func (s *Server) purge(ctx context.Context, userID int64, ids []int64) (int, error) {
	n := 0
	for _, id := range ids {
		var rel, sha string
		var orig sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT path, sha256, orig_path FROM assets
			WHERE id = ? AND user_id = ? AND trashed_at IS NOT NULL AND deleted_at IS NULL`, id, userID).Scan(&rel, &sha, &orig)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return n, err
		}
		if err := s.tombstone(ctx, id); err != nil {
			return n, err
		}
		for _, p := range []string{rel, orig.String} {
			if p == "" {
				continue
			}
			if err := os.Remove(filepath.Join(s.originals, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
				s.log.Warn("removing file", "path", p, "err", err)
			}
		}
		s.removeThumbsIfUnused(ctx, sha)
		n++
	}
	return n, nil
}

func (s *Server) tombstone(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := db.NextSeq(ctx, tx)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET deleted_at = ?, path = '', orig_path = NULL,
		seq = ?, updated_at = ? WHERE id = ?`, now, seq, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_hashes WHERE asset_id = ?`, id); err != nil {
		return err
	}
	if err := unlistAsset(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// handleFile serves the asset's current file: the edit if there is one.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	a, ok := s.readableAssetFromPath(w, r)
	if !ok {
		return
	}
	name := a.Name
	if a.EditedAt != nil {
		name = path.Base(a.path) // the edit may have another extension
	}
	s.serveFile(w, r, a.path, a.MIME, name, a.SHA256)
}

// handleOriginal serves the file as first uploaded, even after edits.
func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	a, ok := s.readableAssetFromPath(w, r)
	if !ok {
		return
	}
	if a.EditedAt != nil {
		s.serveFile(w, r, a.origPath, a.OriginalMIME, a.Name, a.OriginalSHA256)
		return
	}
	s.serveFile(w, r, a.path, a.MIME, a.Name, a.SHA256)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, rel, mimeType, name, sha string) {
	f, err := os.Open(filepath.Join(s.originals, filepath.FromSlash(rel)))
	if err != nil {
		s.internal(w, fmt.Errorf("serving %s: %w", rel, err))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("ETag", `"`+sha+`"`)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// ServeContent handles Range, so videos stream and seek.
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// fillPlaces looks up the town for assets that have a position but no place
// yet: everything from before trunk knew places, once. Each gets a new seq,
// so clients learn the place with their next sync.
func (s *Server) fillPlaces(ctx context.Context) {
	type todo struct {
		id       int64
		lat, lon float64
	}
	var list []todo
	rows, err := s.db.QueryContext(ctx, `SELECT id, lat, lon FROM assets
		WHERE place_id IS NULL AND lat IS NOT NULL AND lon IS NOT NULL AND deleted_at IS NULL`)
	if err != nil {
		s.log.Error("places", "err", err)
		return
	}
	for rows.Next() {
		var t todo
		if rows.Scan(&t.id, &t.lat, &t.lon) == nil {
			list = append(list, t)
		}
	}
	rows.Close()
	if len(list) == 0 {
		return
	}
	start := time.Now()
	for i := 0; i < len(list); i += 200 {
		batch := list[i:min(i+200, len(list))]
		err := s.inTx(ctx, func(tx *sql.Tx, _, now int64) error {
			for _, t := range batch {
				seq, err := db.NextSeq(ctx, tx)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE assets SET place_id = ?, seq = ?, updated_at = ? WHERE id = ?`,
					placeOf(&t.lat, &t.lon), seq, now, t.id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("places", "err", err)
			}
			return
		}
	}
	s.log.Info("places looked up", "assets", len(list), "took", time.Since(start).Round(time.Millisecond))
}
