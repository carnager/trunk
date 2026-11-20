package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/carnager/trunk/internal/media"
)

// Thumbnails are keyed by content hash, so two users who own the same file
// share them: thumbs/ab/abcdef…-small.jpg.
func (s *Server) thumbPath(sha string, size media.Size) string {
	return filepath.Join(s.thumbs, sha[:2], sha+"-"+string(size)+".jpg")
}

func (s *Server) haveThumbs(sha string) bool {
	for _, sz := range media.Sizes {
		if _, err := os.Stat(s.thumbPath(sha, sz)); err != nil {
			return false
		}
	}
	return true
}

// refMutex is a per-hash lock that is dropped from the map once nobody holds
// or waits for it.
type refMutex struct {
	sync.Mutex
	refs int // guarded by Server.thumbMu
}

// errThumbFailed means the file could not be rendered recently; retrying on
// every grid scroll would only burn CPU.
var errThumbFailed = errors.New("thumbnail could not be rendered")

const thumbRetry = time.Hour

// ensureThumbs renders all sizes for a unless they exist. Concurrent calls for
// the same content wait for one render.
func (s *Server) ensureThumbs(ctx context.Context, a *Asset) error {
	if s.haveThumbs(a.SHA256) {
		return nil
	}
	s.thumbMu.Lock()
	if t, ok := s.thumbFail[a.SHA256]; ok && time.Since(t) < thumbRetry {
		s.thumbMu.Unlock()
		return errThumbFailed
	}
	l := s.thumbLock[a.SHA256]
	if l == nil {
		l = &refMutex{}
		s.thumbLock[a.SHA256] = l
	}
	l.refs++
	s.thumbMu.Unlock()

	l.Lock()
	defer func() {
		l.Unlock()
		s.thumbMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.thumbLock, a.SHA256)
		}
		s.thumbMu.Unlock()
	}()
	if s.haveThumbs(a.SHA256) {
		return nil // someone else rendered them while we waited
	}
	select {
	case s.thumbSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.thumbSem }()

	start := time.Now()
	err := media.Render(media.Source{
		Path:        filepath.Join(s.originals, filepath.FromSlash(a.path)),
		Kind:        a.Kind,
		Orientation: a.Orientation,
		DurationMS:  a.DurationMS,
	}, func(sz media.Size) string { return s.thumbPath(a.SHA256, sz) })
	if err != nil {
		s.thumbMu.Lock()
		s.thumbFail[a.SHA256] = time.Now()
		s.thumbMu.Unlock()
		s.log.Warn("thumbnail failed", "asset", a.ID, "err", err)
		return errThumbFailed
	}
	s.log.Debug("thumbnails rendered", "asset", a.ID, "dur", time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *Server) thumbWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.thumbQ:
			a, err := s.scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id = ?`, id))
			if err != nil || a.Deleted {
				continue
			}
			s.fingerprint(ctx, a) // renders the thumbnails first
		}
	}
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	size := r.PathValue("size")
	if !media.ValidSize(size) {
		httpError(w, http.StatusBadRequest, "size must be one of small, medium, preview")
		return
	}
	a, ok := s.readableAssetFromPath(w, r)
	if !ok {
		return
	}
	etag := `"` + a.SHA256 + "-" + size + `"`
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if err := s.ensureThumbs(r.Context(), a); err != nil {
		if errors.Is(err, errThumbFailed) {
			httpError(w, http.StatusNotFound, "no thumbnail for this asset")
		}
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, s.thumbPath(a.SHA256, media.Size(size)))
}

func (s *Server) removeThumbsIfUnused(ctx context.Context, sha string) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assets WHERE sha256 = ? AND deleted_at IS NULL`, sha).Scan(&n); err != nil || n > 0 {
		return
	}
	for _, sz := range media.Sizes {
		os.Remove(s.thumbPath(sha, sz))
	}
}

// janitor empties expired trash and abandoned uploads and keeps the daily
// database backup, once an hour.
func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.sweep(ctx)
		s.dailyBackup(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sweep(ctx context.Context) {
	cut := time.Now().AddDate(0, 0, -s.cfg.TrashDays).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id FROM assets
		WHERE trashed_at IS NOT NULL AND trashed_at < ? AND deleted_at IS NULL`, cut)
	if err != nil {
		s.log.Error("sweep", "err", err)
		return
	}
	type item struct{ id, user int64 }
	var due []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.user) == nil {
			due = append(due, it)
		}
	}
	rows.Close()
	for _, it := range due {
		if _, err := s.purge(ctx, it.user, []int64{it.id}); err != nil {
			s.log.Error("emptying trash", "asset", it.id, "err", err)
		}
	}
	if len(due) > 0 {
		s.log.Info("emptied expired trash", "assets", len(due))
	}

	// Unfinished uploads expire after a week, finished ones after a day.
	now := time.Now()
	rows, err = s.db.QueryContext(ctx, `SELECT id FROM uploads
		WHERE (asset_id IS NULL AND updated_at < ?) OR (asset_id IS NOT NULL AND updated_at < ?)`,
		now.AddDate(0, 0, -7).UnixMilli(), now.AddDate(0, 0, -1).UnixMilli())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.dropUpload(id)
	}
	// Leftovers from simple uploads cut off mid-request or a crash mid-ingest.
	// An import may hold files for as long as it runs, which can be days.
	entries, _ := os.ReadDir(s.uploads)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tus-") && !strings.HasSuffix(e.Name(), ".ingest") {
			continue
		}
		maxAge := 24 * time.Hour
		if strings.HasPrefix(e.Name(), "import-") {
			maxAge = 7 * 24 * time.Hour
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > maxAge {
			os.Remove(filepath.Join(s.uploads, e.Name()))
		}
	}
}
