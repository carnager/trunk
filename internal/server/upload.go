package server

// Resumable uploads following the tus 1.0 core protocol with the creation and
// termination extensions (https://tus.io/protocols/resumable-upload).
//
// Metadata keys understood in Upload-Metadata: filename (required), mtime
// (unix ms), sha256 (hex; verified on completion), source_sha256 (hex; see
// handleSimpleUpload). When the last PATCH
// completes the upload, its response carries Trunk-Asset-Id and
// Trunk-Asset-Created; HEAD on a completed upload returns them again, so a
// client that lost the final response can still learn the outcome.

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const tusVersion = "1.0.0"

type upload struct {
	ID       string
	UserID   int64
	Length   int64
	Offset   int64
	Filename string
	SHA256   string
	Source   string
	Mtime    sql.NullInt64
	AssetID  sql.NullInt64
}

func (s *Server) uploadPath(id string) string { return filepath.Join(s.uploads, "tus-"+id) }

func (s *Server) handleTusOptions(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Tus-Resumable", tusVersion)
	h.Set("Tus-Version", tusVersion)
	h.Set("Tus-Extension", "creation,termination")
	h.Set("Tus-Max-Size", strconv.FormatInt(s.cfg.MaxUpload, 10))
	w.WriteHeader(http.StatusNoContent)
}

func tusCheck(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Tus-Resumable", tusVersion)
	if r.Header.Get("Tus-Resumable") != tusVersion {
		w.Header().Set("Tus-Version", tusVersion)
		httpError(w, http.StatusPreconditionFailed, "unsupported Tus-Resumable version")
		return false
	}
	return true
}

func parseTusMetadata(v string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		k, b64, _ := strings.Cut(strings.TrimSpace(pair), " ")
		if k == "" {
			continue
		}
		val, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		m[k] = string(val)
	}
	return m
}

func (s *Server) handleTusCreate(w http.ResponseWriter, r *http.Request) {
	if !tusCheck(w, r) {
		return
	}
	sess := sessionFrom(r)
	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length <= 0 {
		httpError(w, http.StatusBadRequest, "Upload-Length required (deferred length is not supported)")
		return
	}
	if length > s.cfg.MaxUpload {
		httpError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	md := parseTusMetadata(r.Header.Get("Upload-Metadata"))
	if md["filename"] == "" {
		httpError(w, http.StatusBadRequest, "Upload-Metadata must include filename")
		return
	}
	sha, source := strings.ToLower(md["sha256"]), strings.ToLower(md["source_sha256"])
	if (sha != "" && !reSHA.MatchString(sha)) || (source != "" && !reSHA.MatchString(source)) {
		httpError(w, http.StatusBadRequest, "bad sha256 in Upload-Metadata")
		return
	}
	var mtime any
	if t := parseMillis(md["mtime"]); t != nil {
		mtime = t.UnixMilli()
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	f, err := os.OpenFile(s.uploadPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		s.internal(w, err)
		return
	}
	f.Close()
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO uploads (id, user_id, length, filename, sha256,
		source_sha256, mtime, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sess.UserID, length, md["filename"], sha, source, mtime, now, now)
	if err != nil {
		os.Remove(s.uploadPath(id))
		s.internal(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/uploads/"+id)
	w.Header().Set("Upload-Offset", "0")
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) loadUpload(w http.ResponseWriter, r *http.Request) (*upload, bool) {
	var u upload
	err := s.db.QueryRowContext(r.Context(), `SELECT id, user_id, length, offset, filename, sha256, source_sha256,
		mtime, asset_id FROM uploads WHERE id = ? AND user_id = ?`, r.PathValue("id"), sessionFrom(r).UserID).
		Scan(&u.ID, &u.UserID, &u.Length, &u.Offset, &u.Filename, &u.SHA256, &u.Source, &u.Mtime, &u.AssetID)
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound) // tus: HEAD responses have no body
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return &u, true
}

func (s *Server) handleTusHead(w http.ResponseWriter, r *http.Request) {
	if !tusCheck(w, r) {
		return
	}
	u, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Upload-Offset", strconv.FormatInt(u.Offset, 10))
	h.Set("Upload-Length", strconv.FormatInt(u.Length, 10))
	if u.AssetID.Valid {
		h.Set("Trunk-Asset-Id", strconv.FormatInt(u.AssetID.Int64, 10))
	}
	w.WriteHeader(http.StatusOK)
}

// lockUpload makes sure only one PATCH writes to an upload at a time.
func (s *Server) lockUpload(id string) bool {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if s.uploadBusy[id] {
		return false
	}
	s.uploadBusy[id] = true
	return true
}

func (s *Server) unlockUpload(id string) {
	s.uploadMu.Lock()
	delete(s.uploadBusy, id)
	s.uploadMu.Unlock()
}

func (s *Server) handleTusPatch(w http.ResponseWriter, r *http.Request) {
	if !tusCheck(w, r) {
		return
	}
	if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/offset+octet-stream")
		return
	}
	if !s.lockUpload(r.PathValue("id")) {
		httpError(w, http.StatusConflict, "upload is busy with another request")
		return
	}
	defer s.unlockUpload(r.PathValue("id"))
	u, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	off, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || off != u.Offset {
		w.Header().Set("Upload-Offset", strconv.FormatInt(u.Offset, 10))
		httpError(w, http.StatusConflict, "Upload-Offset does not match")
		return
	}
	if u.AssetID.Valid || u.Offset == u.Length {
		w.Header().Set("Upload-Offset", strconv.FormatInt(u.Offset, 10))
		httpError(w, http.StatusConflict, "upload already complete")
		return
	}

	f, err := os.OpenFile(s.uploadPath(u.ID), os.O_WRONLY, 0)
	if err != nil {
		s.internal(w, err)
		return
	}
	// Drop anything past the recorded offset: a previous PATCH may have
	// written bytes it never got to account for.
	if err := f.Truncate(u.Offset); err != nil {
		f.Close()
		s.internal(w, err)
		return
	}
	if _, err := f.Seek(u.Offset, io.SeekStart); err != nil {
		f.Close()
		s.internal(w, err)
		return
	}
	// Read one byte past the declared length to notice clients that overshoot.
	n, copyErr := io.Copy(f, io.LimitReader(r.Body, u.Length-u.Offset+1))
	syncErr := f.Sync()
	f.Close()
	if u.Offset+n > u.Length {
		f2, _ := os.OpenFile(s.uploadPath(u.ID), os.O_WRONLY, 0)
		if f2 != nil {
			f2.Truncate(u.Offset)
			f2.Close()
		}
		httpError(w, http.StatusRequestEntityTooLarge, "more data than Upload-Length")
		return
	}
	if syncErr != nil {
		s.internal(w, syncErr)
		return
	}
	// Keep what arrived even if the connection broke; the client resumes from here.
	u.Offset += n
	if _, err := s.db.ExecContext(r.Context(), `UPDATE uploads SET offset = ?, updated_at = ? WHERE id = ?`,
		u.Offset, time.Now().UnixMilli(), u.ID); err != nil {
		s.internal(w, err)
		return
	}
	if copyErr != nil {
		s.log.Info("upload interrupted", "id", u.ID, "offset", u.Offset, "err", copyErr)
		return // the connection is gone; nobody reads a response
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(u.Offset, 10))
	if u.Offset < u.Length {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.completeUpload(w, r, u)
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, u *upload) {
	sess := sessionFrom(r)
	tmp := s.uploadPath(u.ID)
	sum, err := hashFile(tmp)
	if err != nil {
		s.internal(w, err)
		return
	}
	if u.SHA256 != "" && u.SHA256 != sum {
		// The data is corrupt; start over rather than resume into garbage.
		s.dropUpload(u.ID)
		httpError(w, http.StatusBadRequest, errHashMismatch.Error())
		return
	}
	// Move the data aside so ingest owns it and the tus file name stays free.
	work := tmp + ".ingest"
	if err := os.Rename(tmp, work); err != nil {
		s.internal(w, err)
		return
	}
	var mtime *time.Time
	if u.Mtime.Valid {
		t := time.UnixMilli(u.Mtime.Int64)
		mtime = &t
	}
	ok := s.finishIngest(w, r, ingestReq{
		UserID: sess.UserID, UserName: sess.UserName, TmpPath: work, Name: u.Filename, SHA256: sum, Source: u.Source, Mtime: mtime,
	}, func(a *Asset, created bool) {
		s.db.ExecContext(r.Context(), `UPDATE uploads SET asset_id = ?, updated_at = ? WHERE id = ?`,
			a.ID, time.Now().UnixMilli(), u.ID)
		w.Header().Set("Trunk-Asset-Id", strconv.FormatInt(a.ID, 10))
		w.Header().Set("Trunk-Asset-Created", strconv.FormatBool(created))
	})
	if !ok {
		s.dropUpload(u.ID) // the data is gone; a retry has to start fresh
	}
}

func (s *Server) dropUpload(id string) {
	os.Remove(s.uploadPath(id))
	s.db.Exec(`DELETE FROM uploads WHERE id = ?`, id)
}

func (s *Server) handleTusDelete(w http.ResponseWriter, r *http.Request) {
	if !tusCheck(w, r) {
		return
	}
	if !s.lockUpload(r.PathValue("id")) {
		httpError(w, http.StatusConflict, "upload is busy with another request")
		return
	}
	defer s.unlockUpload(r.PathValue("id"))
	u, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	s.dropUpload(u.ID)
	w.WriteHeader(http.StatusNoContent)
}
