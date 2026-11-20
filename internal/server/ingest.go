package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/carnager/trunk/internal/db"
	"github.com/carnager/trunk/internal/media"
)

// ingestReq describes a finished upload waiting in the uploads directory.
type ingestReq struct {
	UserID   int64
	UserName string
	TmpPath  string     // removed or moved into the library by ingest
	Name     string     // original file name from the client
	SHA256   string     // computed hash
	Mtime    *time.Time // client-side file time, last resort for taken_at
}

var errHashMismatch = errors.New("sha256 does not match the uploaded data")

// ingest moves an uploaded file into the library and records it. If the user
// already has a file with the same content, the upload is dropped and the
// existing asset returned with created=false.
func (s *Server) ingest(ctx context.Context, q ingestReq) (a *Asset, created bool, err error) {
	defer func() {
		if !created {
			os.Remove(q.TmpPath)
		}
	}()
	if id, ok, err := s.findBySHA(ctx, q.UserID, q.SHA256); err != nil {
		return nil, false, err
	} else if ok {
		a, err := s.getAsset(ctx, q.UserID, id)
		return a, false, err
	}

	info, err := media.Extract(q.TmpPath, q.Name)
	if err != nil {
		return nil, false, err
	}
	st, err := os.Stat(q.TmpPath)
	if err != nil {
		return nil, false, err
	}
	taken := info.TakenAt
	if taken.IsZero() && q.Mtime != nil {
		taken = *q.Mtime
	}
	if taken.IsZero() {
		taken = time.Now()
	}
	// Folder by local date where known, so a photo from New Year's Eve in Tokyo
	// lands in December.
	local := taken.UTC()
	if info.TZOffset != nil {
		local = local.Add(time.Duration(*info.TZOffset) * time.Minute)
	}

	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	// Check again: a parallel upload of the same file may have won the race.
	if id, ok, err := s.findBySHA(ctx, q.UserID, q.SHA256); err != nil {
		return nil, false, err
	} else if ok {
		a, err := s.getAsset(ctx, q.UserID, id)
		return a, false, err
	}

	name := safeName(q.Name, info.MIME)
	dir := path.Join(safeName(q.UserName, ""), local.Format("2006"), local.Format("01"))
	if err := os.MkdirAll(filepath.Join(s.originals, filepath.FromSlash(dir)), 0o750); err != nil {
		return nil, false, err
	}
	rel, err := s.place(q.TmpPath, dir, name)
	if err != nil {
		return nil, false, err
	}
	abs := filepath.Join(s.originals, filepath.FromSlash(rel))
	os.Chtimes(abs, taken, taken) // file managers then sort by capture time too

	id, err := s.insertAsset(ctx, q, rel, info, st.Size(), taken)
	if err != nil {
		os.Rename(abs, q.TmpPath) // put it back so the deferred cleanup removes it
		return nil, false, err
	}
	created = true
	select {
	case s.thumbQ <- id:
	default: // queue full; thumbnails are rendered on first request instead
	}
	s.log.Info("asset added", "user", q.UserName, "id", id, "path", rel)
	a, err = s.getAsset(ctx, q.UserID, id)
	return a, true, err
}

func (s *Server) findBySHA(ctx context.Context, userID int64, sha string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM assets WHERE user_id = ? AND sha256 = ? AND deleted_at IS NULL`, userID, sha).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// place moves tmp to dir/name inside the originals, adding _1, _2, ... until
// the name is free. It returns the slash-separated path relative to originals.
func (s *Server) place(tmp, dir, name string) (string, error) {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		cand := name
		if i > 0 {
			cand = base + "_" + strconv.Itoa(i) + ext
		}
		rel := path.Join(dir, cand)
		abs := filepath.Join(s.originals, filepath.FromSlash(rel))
		if _, err := os.Lstat(abs); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Rename(tmp, abs); err != nil {
			return "", err
		}
		return rel, nil
	}
	return "", fmt.Errorf("no free file name for %s in %s", name, dir)
}

func (s *Server) insertAsset(ctx context.Context, q ingestReq, rel string, in media.Info, size int64, taken time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seq, err := db.NextSeq(ctx, tx)
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `INSERT INTO assets (user_id, seq, sha256, path, orig_name, kind, mime, size,
		taken_at, tz_offset, width, height, orientation, duration_ms, lat, lon, alt, make, model, lens,
		f_number, exposure, iso, focal_length, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.UserID, seq, q.SHA256, rel, q.Name, in.Kind, in.MIME, size,
		taken.UnixMilli(), in.TZOffset, nz(in.Width), nz(in.Height), nz(in.Orientation), nz(in.DurationMS),
		in.Lat, in.Lon, in.Alt, ns(in.Make), ns(in.Model), ns(in.Lens),
		nzf(in.FNumber), nzf(in.Exposure), nz(in.ISO), nzf(in.FocalLength), now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func nz[T int | int64](v T) any {
	if v == 0 {
		return nil
	}
	return v
}

func nzf(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func ns(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// safeName turns a client-supplied name into one safe to use as a single
// path element. mime picks an extension when the name has none.
func safeName(name, mime string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == 0 || unicode.IsControl(r) || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if len(name) > 180 {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	if name == "" {
		name = "asset"
	}
	if mime != "" && path.Ext(name) == "" {
		if ext := extFor[mime]; ext != "" {
			name += ext
		}
	}
	return name
}

var extFor = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"image/heic": ".heic", "image/avif": ".avif", "image/bmp": ".bmp", "image/x-adobe-dng": ".dng",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "video/3gpp": ".3gp", "video/webm": ".webm",
	"video/x-matroska": ".mkv", "video/avi": ".avi",
}

// receive streams body into a new file in the uploads directory, hashing it
// on the way, and returns the file's path and hash.
func (s *Server) receive(body io.Reader) (tmp, sum string, n int64, err error) {
	f, err := os.CreateTemp(s.uploads, "simple-*")
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", 0, err
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// handleSimpleUpload takes a whole file as the request body:
//
//	POST /api/v1/assets?filename=IMG_1234.jpg&mtime=<unix ms>
//	X-Sha256: <hex>   (optional; verified when present)
//
// Use it for photos; large videos over mobile networks belong on tus.
func (s *Server) handleSimpleUpload(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	name := r.URL.Query().Get("filename")
	if name == "" {
		httpError(w, http.StatusBadRequest, "filename is required")
		return
	}
	want := strings.ToLower(r.Header.Get("X-Sha256"))
	if want != "" && !reSHA.MatchString(want) {
		httpError(w, http.StatusBadRequest, "bad X-Sha256")
		return
	}
	if want != "" {
		if id, ok, err := s.findBySHA(r.Context(), sess.UserID, want); err != nil {
			s.internal(w, err)
			return
		} else if ok {
			// Already there; skip reading the body at all.
			a, err := s.getAsset(r.Context(), sess.UserID, id)
			if err != nil {
				s.internal(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"asset": a, "created": false})
			return
		}
	}
	tmp, sum, _, err := s.receive(http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpError(w, http.StatusRequestEntityTooLarge, "file too large")
			return
		}
		s.log.Warn("upload aborted", "user", sess.UserName, "err", err)
		httpError(w, http.StatusBadRequest, "upload incomplete")
		return
	}
	if want != "" && want != sum {
		os.Remove(tmp)
		httpError(w, http.StatusBadRequest, errHashMismatch.Error())
		return
	}
	s.finishIngest(w, r, ingestReq{
		UserID: sess.UserID, UserName: sess.UserName, TmpPath: tmp, Name: name, SHA256: sum,
		Mtime: parseMillis(r.URL.Query().Get("mtime")),
	}, nil)
}

// finishIngest runs ingest and writes the response; it reports success. For
// tus, tus is called with the result to set headers before an empty 204.
func (s *Server) finishIngest(w http.ResponseWriter, r *http.Request, q ingestReq, tus func(*Asset, bool)) bool {
	a, created, err := s.ingest(r.Context(), q)
	if errors.Is(err, media.ErrUnsupported) {
		httpError(w, http.StatusUnsupportedMediaType, "not a photo or video")
		return false
	} else if err != nil {
		s.internal(w, err)
		return false
	}
	if tus != nil {
		tus(a, created)
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"asset": a, "created": created})
	return true
}

func parseMillis(v string) *time.Time {
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms)
	return &t
}
