package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/carnager/trunk/internal/db"
	"github.com/carnager/trunk/internal/media"
)

// motionFile is the file that holds a motion photo's video: one of its own,
// or else the first upload, since an edit is a plain picture.
func (a *Asset) motionFile() string {
	if a.motionPath != "" {
		return a.motionPath
	}
	if a.origPath != "" {
		return a.origPath
	}
	return a.path
}

// fillMotion looks for motion photos among the JPEGs uploaded before trunk
// knew them, once. Those found get a new seq, so clients learn with their
// next sync; the others are only marked as looked at.
func (s *Server) fillMotion(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE motion_offset IS NULL AND motion_path IS NULL AND deleted_at IS NULL`)
	if err != nil {
		s.log.Error("motion photos", "err", err)
		return
	}
	var todo []*Asset
	for rows.Next() {
		if a, err := s.scanAsset(rows); err == nil {
			todo = append(todo, a)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return
	}
	start := time.Now()
	found := 0
	for _, a := range todo {
		if ctx.Err() != nil {
			return
		}
		var off, n int64
		mime := a.MIME
		if a.OriginalMIME != "" {
			mime = a.OriginalMIME
		}
		var sum string
		if mime == "image/jpeg" {
			abs := filepath.Join(s.originals, filepath.FromSlash(a.motionFile()))
			if o, l, ok, _ := media.MotionVideo(abs); ok {
				off, n = o, l
				sum, _ = media.HashSection(abs, o, l)
			}
		}
		err := s.inTx(ctx, func(tx *sql.Tx, _, now int64) error {
			if n == 0 {
				_, err := tx.ExecContext(ctx, `UPDATE assets SET motion_offset = 0, motion_length = 0 WHERE id = ?`, a.ID)
				return err
			}
			seq, err := db.NextSeq(ctx, tx)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE assets SET motion_offset = ?, motion_length = ?, motion_sha256 = ?, seq = ?, updated_at = ? WHERE id = ?`,
				off, n, ns(sum), seq, now, a.ID)
			return err
		})
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("motion photos", "err", err)
			}
			return
		}
		if n > 0 {
			found++
		}
	}
	s.log.Info("motion photos looked for", "assets", len(todo), "found", found, "took", time.Since(start).Round(time.Millisecond))
}

// fillMotionHashes hashes the videos of motion photos found before trunk
// kept their hash, once.
func (s *Server) fillMotionHashes(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE motion_length > 0 AND motion_sha256 IS NULL AND deleted_at IS NULL`)
	if err != nil {
		s.log.Error("motion hashes", "err", err)
		return
	}
	var todo []*Asset
	for rows.Next() {
		if a, err := s.scanAsset(rows); err == nil {
			todo = append(todo, a)
		}
	}
	rows.Close()
	for _, a := range todo {
		if ctx.Err() != nil {
			return
		}
		sum, err := media.HashSection(filepath.Join(s.originals, filepath.FromSlash(a.motionFile())), a.motionOff, a.motionLen)
		if err != nil {
			continue
		}
		s.db.ExecContext(ctx, `UPDATE assets SET motion_sha256 = ? WHERE id = ?`, sum, a.ID)
	}
}

// handleMotion: GET /api/v1/assets/{id}/motion → the video of a motion photo
// (video/mp4, seekable), 404 for a still.
func (s *Server) handleMotion(w http.ResponseWriter, r *http.Request) {
	a, ok := s.readableAssetFromPath(w, r)
	if !ok {
		return
	}
	s.serveMotion(w, r, a)
}

func (s *Server) serveMotion(w http.ResponseWriter, r *http.Request, a *Asset) {
	if !a.Motion {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.originals, filepath.FromSlash(a.motionFile())))
	if err != nil {
		s.internal(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "", st.ModTime(), io.NewSectionReader(f, a.motionOff, a.motionLen))
}

// motionSuffix names a motion photo's video kept as a file of its own:
// photo.jpg → photo.motion.mp4, next to it.
const motionSuffix = ".motion.mp4"

func motionPathFor(photo string) string {
	return strings.TrimSuffix(photo, path.Ext(photo)) + motionSuffix
}

// errHasMotion: the photo already has its video.
var errHasMotion = errors.New("photo already has its motion video")

// attachMotion makes the MP4 at tmp (in the data directory) the motion video
// of photo a, which has none: an export had split it off. It is moved next
// to the photo.
func (s *Server) attachMotion(ctx context.Context, a *Asset, tmp string) error {
	if a.Motion {
		return errHasMotion
	}
	if a.Kind != "image" {
		return errNotPhoto
	}
	if _, kind, err := media.Sniff(tmp, "motion.mp4"); err != nil || kind != "video" {
		return fmt.Errorf("not a video")
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	sum, err := hashFile(tmp)
	if err != nil {
		return err
	}
	photo := a.path
	if a.origPath != "" {
		photo = a.origPath
	}
	rel := motionPathFor(photo)
	abs := filepath.Join(s.originals, filepath.FromSlash(rel))
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("%s exists", rel)
	}
	if err := os.Rename(tmp, abs); err != nil {
		return err
	}
	err = s.inTx(ctx, func(tx *sql.Tx, _, now int64) error {
		seq, err := db.NextSeq(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE assets SET motion_path = ?, motion_offset = 0, motion_length = ?, motion_sha256 = ?, seq = ?, updated_at = ?
			WHERE id = ? AND deleted_at IS NULL AND motion_path IS NULL AND COALESCE(motion_length, 0) = 0`, rel, st.Size(), sum, seq, now, a.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errHasMotion
		}
		return nil
	})
	if err != nil {
		os.Remove(abs)
	}
	return err
}
