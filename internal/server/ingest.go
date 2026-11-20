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
	Source   string     // optional: hash of the client's file before it shrank it
	Mtime    *time.Time // client-side file time, last resort for taken_at
	Hints    *Hints     // what another source (an import) knows about the file
}

// Hints fill in what a file lacks. They never override the file's own
// metadata, which is usually more precise (a local time, a GPS fix).
type Hints struct {
	TakenAt  time.Time // an exact instant
	Lat, Lon *float64
	Alt      *float64
	Favorite bool
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
	if a, err := s.existing(ctx, q); a != nil || err != nil {
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
	if h := q.Hints; h != nil {
		applyHints(&info, h)
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
	if a, err := s.existing(ctx, q); a != nil || err != nil {
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

	id, err := s.insertAsset(ctx, q, rel, info, st.Size(), taken, q.Hints != nil && q.Hints.Favorite)
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

func applyHints(in *media.Info, h *Hints) {
	switch {
	case h.TakenAt.IsZero(), in.TakenFrom == "video":
	case in.TakenFrom == "exif":
		// Old cameras write local time without an offset; the instant gives it.
		if in.TZOffset == nil {
			if off := media.OffsetFrom(in.TakenAt, h.TakenAt, 0); off != nil {
				in.TZOffset = off
				in.TakenAt = in.TakenAt.Add(-time.Duration(*off) * time.Minute)
			}
		}
	case in.TakenFrom == "filename":
		// The name has the local wall clock, the hint the instant: together
		// they give the offset, and the instant is exact either way.
		in.TZOffset = media.OffsetFrom(in.TakenAt, h.TakenAt, in.DurationMS)
		in.TakenAt = h.TakenAt
	default:
		in.TakenAt = h.TakenAt
	}
	if in.Lat == nil && h.Lat != nil && h.Lon != nil {
		in.Lat, in.Lon, in.Alt = h.Lat, h.Lon, h.Alt
	}
}

// existing returns the asset the upload duplicates, by its own hash or its
// source hash, or nil.
func (s *Server) existing(ctx context.Context, q ingestReq) (*Asset, error) {
	for _, h := range []string{q.SHA256, q.Source} {
		if h == "" {
			continue
		}
		id, ok, err := s.findBySHA(ctx, q.UserID, h)
		if err != nil {
			return nil, err
		}
		if ok {
			return s.getAsset(ctx, q.UserID, id)
		}
	}
	return nil, nil
}

// findBySHA looks a hash up among everything the user's assets are known by.
func (s *Server) findBySHA(ctx context.Context, userID int64, sha string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT asset_id FROM asset_hashes WHERE user_id = ? AND sha256 = ?`, userID, sha).Scan(&id)
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
		// A hard link fails if the name is taken, so two processes (the server
		// and an import) can never overwrite each other's file.
		if err := os.Link(tmp, abs); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		os.Remove(tmp)
		return rel, nil
	}
	return "", fmt.Errorf("no free file name for %s in %s", name, dir)
}

func (s *Server) insertAsset(ctx context.Context, q ingestReq, rel string, in media.Info, size int64, taken time.Time, fav bool) (int64, error) {
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
		f_number, exposure, iso, focal_length, favorite, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.UserID, seq, q.SHA256, rel, q.Name, in.Kind, in.MIME, size,
		taken.UnixMilli(), in.TZOffset, nz(in.Width), nz(in.Height), nz(in.Orientation), nz(in.DurationMS),
		in.Lat, in.Lon, in.Alt, ns(in.Make), ns(in.Model), ns(in.Lens),
		nzf(in.FNumber), nzf(in.Exposure), nz(in.ISO), nzf(in.FocalLength), boolInt(fav), now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := addHash(ctx, tx, q.UserID, q.SHA256, id, "original"); err != nil {
		return 0, err
	}
	if q.Source != "" && q.Source != q.SHA256 {
		if err := addHash(ctx, tx, q.UserID, q.Source, id, "source"); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

func addHash(ctx context.Context, x db.Execer, userID int64, sha string, assetID int64, kind string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO asset_hashes (user_id, sha256, asset_id, kind) VALUES (?, ?, ?, ?)`,
		userID, sha, assetID, kind)
	return err
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
//	POST /api/v1/assets?filename=IMG_1234.jpg&mtime=<unix ms>&source_sha256=<hex>
//	X-Sha256: <hex>   (optional; verified when present)
//
// source_sha256 is for clients that shrink photos before sending: the hash of
// the file as it is on the device, so it is recognised again later.
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
	source := strings.ToLower(r.URL.Query().Get("source_sha256"))
	if source != "" && !reSHA.MatchString(source) {
		httpError(w, http.StatusBadRequest, "bad source_sha256")
		return
	}
	// Already there? Then the body need not be read at all.
	if a, err := s.existing(r.Context(), ingestReq{UserID: sess.UserID, SHA256: want, Source: source}); err != nil {
		s.internal(w, err)
		return
	} else if a != nil {
		writeJSON(w, http.StatusOK, map[string]any{"asset": a, "created": false})
		return
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
		UserID: sess.UserID, UserName: sess.UserName, TmpPath: tmp, Name: name, SHA256: sum, Source: source,
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
