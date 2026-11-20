package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/carnager/trunk/internal/db"
	"github.com/carnager/trunk/internal/media"
)

// Edits replace what an asset shows without losing what was uploaded: the
// first upload stays on disk (orig_* columns) and /original keeps serving it.
// Size, type, dimensions and thumbnails follow the edit; when and where the
// photo was taken, and with which camera, stay as they were.

// handlePutEdit stores the request body as the asset's current version.
//
//	PUT /api/v1/assets/{id}/edit
//	X-Sha256: <hex>   (optional; verified when present)
func (s *Server) handlePutEdit(w http.ResponseWriter, r *http.Request) {
	a, ok := s.assetFromPath(w, r)
	if !ok {
		return
	}
	if a.Kind != "image" {
		httpError(w, http.StatusBadRequest, "only photos can be edited")
		return
	}
	want := strings.ToLower(r.Header.Get("X-Sha256"))
	if want != "" && !reSHA.MatchString(want) {
		httpError(w, http.StatusBadRequest, "bad X-Sha256")
		return
	}
	tmp, sum, _, err := s.receive(http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload))
	if err != nil {
		httpError(w, http.StatusBadRequest, "upload incomplete")
		return
	}
	defer os.Remove(tmp) // a no-op once the file has moved into the library
	if want != "" && want != sum {
		httpError(w, http.StatusBadRequest, errHashMismatch.Error())
		return
	}

	switch {
	case sum == a.SHA256:
		writeJSON(w, http.StatusOK, a) // nothing changed
		return
	case a.EditedAt != nil && sum == a.OriginalSHA256:
		s.revert(w, r, a) // edited back to exactly the original
		return
	}
	if id, ok, err := s.findBySHA(r.Context(), a.userID, sum); err != nil {
		s.internal(w, err)
		return
	} else if ok && id != a.ID {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this file is already another asset", "asset_id": id})
		return
	}

	info, err := media.Extract(tmp, a.Name)
	if errors.Is(err, media.ErrUnsupported) || (err == nil && info.Kind != "image") {
		httpError(w, http.StatusUnsupportedMediaType, "not a photo")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	st, err := os.Stat(tmp)
	if err != nil {
		s.internal(w, err)
		return
	}

	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	// Next to the original, so the folder shows both: IMG_1234.heic, IMG_1234_edit.jpg.
	first := a.path
	if a.EditedAt != nil {
		first = a.origPath
	}
	base := strings.TrimSuffix(path.Base(first), path.Ext(first))
	name := safeName(base+"_edit", info.MIME)
	if ext := extFor[info.MIME]; ext != "" && path.Ext(name) != ext {
		name += ext
	}
	rel, err := s.place(tmp, path.Dir(first), name)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err := s.applyEdit(r.Context(), a, rel, sum, st.Size(), info); err != nil {
		os.Remove(filepath.Join(s.originals, filepath.FromSlash(rel)))
		s.internal(w, err)
		return
	}
	if a.EditedAt != nil {
		// The previous edit is replaced, not kept: only the first upload is sacred.
		os.Remove(filepath.Join(s.originals, filepath.FromSlash(a.path)))
	}
	s.removeThumbsIfUnused(r.Context(), a.SHA256)
	select {
	case s.thumbQ <- a.ID:
	default:
	}
	s.log.Info("asset edited", "id", a.ID, "path", rel)
	s.respondAsset(w, r, a)
}

func (s *Server) applyEdit(ctx context.Context, a *Asset, rel, sha string, size int64, in media.Info) error {
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
	// SQLite evaluates every right-hand side against the row as it was, so the
	// first edit copies the current file into orig_* and later edits keep it.
	_, err = tx.ExecContext(ctx, `UPDATE assets SET
		orig_path        = CASE WHEN edited_at IS NULL THEN path        ELSE orig_path END,
		orig_sha256      = CASE WHEN edited_at IS NULL THEN sha256      ELSE orig_sha256 END,
		orig_size        = CASE WHEN edited_at IS NULL THEN size        ELSE orig_size END,
		orig_mime        = CASE WHEN edited_at IS NULL THEN mime        ELSE orig_mime END,
		orig_width       = CASE WHEN edited_at IS NULL THEN width       ELSE orig_width END,
		orig_height      = CASE WHEN edited_at IS NULL THEN height      ELSE orig_height END,
		orig_orientation = CASE WHEN edited_at IS NULL THEN orientation ELSE orig_orientation END,
		path = ?, sha256 = ?, size = ?, mime = ?, width = ?, height = ?, orientation = ?,
		edited_at = ?, seq = ?, updated_at = ?
		WHERE id = ?`,
		rel, sha, size, in.MIME, nz(in.Width), nz(in.Height), nz(in.Orientation), now, seq, now, a.ID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_hashes WHERE asset_id = ? AND kind = 'edit'`, a.ID); err != nil {
		return err
	}
	// The hash may already point here as the client's pre-shrink source.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO asset_hashes (user_id, sha256, asset_id, kind)
		VALUES (?, ?, ?, 'edit')`, a.userID, sha, a.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// handleDeleteEdit goes back to the first upload.
func (s *Server) handleDeleteEdit(w http.ResponseWriter, r *http.Request) {
	a, ok := s.assetFromPath(w, r)
	if !ok {
		return
	}
	if a.EditedAt == nil {
		writeJSON(w, http.StatusOK, a)
		return
	}
	s.revert(w, r, a)
}

func (s *Server) revert(w http.ResponseWriter, r *http.Request, a *Asset) {
	ctx := r.Context()
	err := func() error {
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
		if _, err := tx.ExecContext(ctx, `UPDATE assets SET
			path = orig_path, sha256 = orig_sha256, size = orig_size, mime = orig_mime,
			width = orig_width, height = orig_height, orientation = orig_orientation,
			orig_path = NULL, orig_sha256 = NULL, orig_size = NULL, orig_mime = NULL,
			orig_width = NULL, orig_height = NULL, orig_orientation = NULL,
			edited_at = NULL, seq = ?, updated_at = ?
			WHERE id = ? AND edited_at IS NOT NULL`, seq, now, a.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM asset_hashes WHERE asset_id = ? AND kind = 'edit'`, a.ID); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		s.internal(w, err)
		return
	}
	os.Remove(filepath.Join(s.originals, filepath.FromSlash(a.path)))
	s.removeThumbsIfUnused(ctx, a.SHA256)
	s.log.Info("edit reverted", "id", a.ID)
	s.respondAsset(w, r, a)
}

func (s *Server) respondAsset(w http.ResponseWriter, r *http.Request, a *Asset) {
	fresh, err := s.getAsset(r.Context(), a.userID, a.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}
