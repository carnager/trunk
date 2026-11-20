package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/carnager/trunk/internal/media"
)

// Public links show photos to people without an account: an album as it is
// now, or a fixed selection. Anyone with the link sees them; nothing else of
// the library. The page is served by trunk itself, without anything loaded
// from elsewhere. Originals carry their EXIF data, GPS included, so whether
// they may be downloaded is a choice made per link; the page itself shows
// previews and never coordinates.

// Link is a public link as its owner sees it.
type Link struct {
	Token     string     `json:"token"`
	Path      string     `json:"path"` // append to the server's address
	Title     string     `json:"title"`
	AlbumID   int64      `json:"album_id,omitempty"`
	AssetIDs  []int64    `json:"asset_ids,omitempty"` // a selection link's photos
	Download  bool       `json:"download"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func newLinkToken() string {
	b := make([]byte, 16) // 128 bits: not to be guessed
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// handleCreateLink: POST /api/v1/links {"album_id"} or {"asset_ids": [..]},
// optional "title", "download" (default true), "expires_days".
func (s *Server) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := sessionFrom(r).UserID
	var req struct {
		AlbumID     int64   `json:"album_id"`
		AssetIDs    []int64 `json:"asset_ids"`
		Title       string  `json:"title"`
		Download    *bool   `json:"download"`
		ExpiresDays int     `json:"expires_days"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if (req.AlbumID == 0) == (len(req.AssetIDs) == 0) || len(req.AssetIDs) > 5000 || req.ExpiresDays < 0 {
		httpError(w, http.StatusBadRequest, "give album_id or 1 to 5000 asset_ids")
		return
	}
	title := strings.TrimSpace(req.Title)
	if req.AlbumID != 0 {
		// Only one's own albums: what others share stays within the household.
		var name string
		err := s.db.QueryRowContext(ctx, `SELECT name FROM albums WHERE id = ? AND user_id = ? AND kind = 'album'
			AND deleted_at IS NULL`, req.AlbumID, me).Scan(&name)
		if err != nil {
			httpError(w, http.StatusNotFound, "no such album of yours")
			return
		}
		if title == "" {
			title = name
		}
	} else {
		for _, id := range req.AssetIDs {
			var owner int64
			if err := s.db.QueryRowContext(ctx, `SELECT user_id FROM assets WHERE id = ? AND deleted_at IS NULL`, id).Scan(&owner); err != nil || owner != me {
				httpError(w, http.StatusBadRequest, "links can only show your own photos")
				return
			}
		}
		if title == "" {
			title = "Fotos"
		}
	}
	if len(title) > 200 {
		title = title[:200]
	}
	token := newLinkToken()
	now := time.Now()
	var expires any
	if req.ExpiresDays > 0 {
		expires = now.AddDate(0, 0, req.ExpiresDays).UnixMilli()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	var album any
	if req.AlbumID != 0 {
		album = req.AlbumID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO links (token, user_id, album_id, title, download, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, token, me, album, title, boolInt(req.Download == nil || *req.Download), now.UnixMilli(), expires); err != nil {
		s.internal(w, err)
		return
	}
	for i, id := range req.AssetIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO link_assets (token, asset_id, pos) VALUES (?, ?, ?)`, token, id, i); err != nil {
			s.internal(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	s.log.Info("link created", "user", sessionFrom(r).UserName, "title", title)
	links, err := s.links(ctx, me, token)
	if err != nil || len(links) == 0 {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, links[0])
}

// links lists a user's live links, or just the one with token.
func (s *Server) links(ctx context.Context, userID int64, token string) ([]Link, error) {
	q := `SELECT token, title, COALESCE(album_id, 0), download, created_at, expires_at FROM links
		WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`
	args := []any{userID, time.Now().UnixMilli()}
	if token != "" {
		q += ` AND token = ?`
		args = append(args, token)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	var list []Link
	for rows.Next() {
		var l Link
		var created int64
		var expires sql.NullInt64
		if err := rows.Scan(&l.Token, &l.Title, &l.AlbumID, &l.Download, &created, &expires); err != nil {
			rows.Close()
			return nil, err
		}
		l.Path = "/s/" + l.Token
		l.CreatedAt = time.UnixMilli(created).UTC()
		if expires.Valid {
			e := time.UnixMilli(expires.Int64).UTC()
			l.ExpiresAt = &e
		}
		list = append(list, l)
	}
	rows.Close()
	for i := range list {
		if list[i].AlbumID == 0 {
			ids, err := s.linkAssetIDs(ctx, list[i].Token)
			if err != nil {
				return nil, err
			}
			list[i].AssetIDs = ids
		}
	}
	return list, rows.Err()
}

func (s *Server) linkAssetIDs(ctx context.Context, token string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT asset_id FROM link_assets WHERE token = ? ORDER BY pos`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	list, err := s.links(r.Context(), sessionFrom(r).UserID, "")
	if err != nil {
		s.internal(w, err)
		return
	}
	if list == nil {
		list = []Link{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": list})
}

func (s *Server) handleRevokeLink(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.ExecContext(r.Context(), `UPDATE links SET revoked_at = ? WHERE token = ? AND user_id = ? AND revoked_at IS NULL`,
		time.Now().UnixMilli(), r.PathValue("token"), sessionFrom(r).UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "no such link")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- public side ---

type publicLink struct {
	token, title string
	userID       int64
	albumID      int64
	download     bool
}

var errLinkGone = errors.New("link revoked, expired or unknown")

func (s *Server) openLink(ctx context.Context, token string) (*publicLink, error) {
	var l publicLink
	var album sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT token, title, user_id, album_id, download FROM links
		WHERE token = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		token, time.Now().UnixMilli()).Scan(&l.token, &l.title, &l.userID, &album, &l.download)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errLinkGone
	}
	l.albumID = album.Int64
	return &l, err
}

// assets are what the link shows now: the owner's live, untrashed photos of
// the album or the selection, in order.
func (s *Server) linkAssets(ctx context.Context, l *publicLink) ([]*Asset, error) {
	var q string
	var arg any
	if l.albumID != 0 {
		q = `SELECT ` + prefixed("a.", assetCols) + ` FROM album_assets x JOIN assets a ON a.id = x.asset_id
			JOIN albums al ON al.id = x.album_id AND al.deleted_at IS NULL
			WHERE x.album_id = ? AND a.user_id = ? ORDER BY x.pos`
		arg = l.albumID
	} else {
		q = `SELECT ` + prefixed("a.", assetCols) + ` FROM link_assets x JOIN assets a ON a.id = x.asset_id
			WHERE x.token = ? AND a.user_id = ? ORDER BY x.pos`
		arg = l.token
	}
	rows, err := s.db.QueryContext(ctx, q, arg, l.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*Asset
	for rows.Next() {
		a, err := s.scanAsset(rows)
		if err != nil {
			return nil, err
		}
		if a.Deleted || a.TrashedAt != nil {
			continue
		}
		localize(ctx, a)
		list = append(list, a)
	}
	return list, rows.Err()
}

func (s *Server) linkAsset(w http.ResponseWriter, r *http.Request) (*publicLink, *Asset, bool) {
	l, err := s.openLink(r.Context(), r.PathValue("token"))
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	list, err := s.linkAssets(r.Context(), l)
	if err != nil {
		s.internal(w, err)
		return nil, nil, false
	}
	for _, a := range list {
		if a.ID == id {
			return l, a, true
		}
	}
	http.NotFound(w, r)
	return nil, nil, false
}

//go:embed web/share.html
var shareHTML string

var shareTmpl = template.Must(template.New("share").Funcs(template.FuncMap{
	"date": func(a *Asset) string {
		if a.LocalTime == "" {
			return ""
		}
		t, err := time.Parse("2006-01-02T15:04:05", a.LocalTime)
		if err != nil {
			return ""
		}
		return t.Format("2.1.2006")
	},
}).Parse(shareHTML))

// handleSharePage is the page behind a link.
func (s *Server) handleSharePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src 'self'; media-src 'self'")
	l, err := s.openLink(r.Context(), r.PathValue("token"))
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		shareTmpl.Execute(w, map[string]any{"Gone": true})
		return
	}
	list, err := s.linkAssets(r.Context(), l)
	if err != nil {
		s.internal(w, err)
		return
	}
	var owner string
	s.db.QueryRowContext(r.Context(), `SELECT name FROM users WHERE id = ?`, l.userID).Scan(&owner)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-cache")
	shareTmpl.Execute(w, map[string]any{
		"Title": l.title, "Owner": owner, "Assets": list, "Token": l.token, "Download": l.download,
	})
}

func (s *Server) handleShareThumb(w http.ResponseWriter, r *http.Request) {
	size := r.PathValue("size")
	if !media.ValidSize(size) {
		http.NotFound(w, r)
		return
	}
	_, a, ok := s.linkAsset(w, r)
	if !ok {
		return
	}
	if err := s.ensureThumbs(r.Context(), a); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// Short: a revoked link should stop working in browsers soon too.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, s.thumbPath(a.SHA256, media.Size(size)))
}

// handleShareFile serves a photo's file: videos always (to play them), photos
// only where the link allows downloads.
func (s *Server) handleShareFile(w http.ResponseWriter, r *http.Request) {
	l, a, ok := s.linkAsset(w, r)
	if !ok {
		return
	}
	if !l.download && a.Kind != "video" {
		http.NotFound(w, r)
		return
	}
	name := a.Name
	if a.EditedAt != nil {
		name = path.Base(a.path)
	}
	disp := "inline"
	if r.URL.Query().Has("dl") {
		if !l.download {
			http.NotFound(w, r)
			return
		}
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	s.serveFileRaw(w, r, a.path, a.MIME)
}
