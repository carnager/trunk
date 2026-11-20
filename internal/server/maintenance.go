package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carnager/trunk/internal/media"
)

// --- backup ---

// Backup writes a consistent copy of the database to dest, also while the
// server runs. It must not exist yet.
func (s *Server) Backup(ctx context.Context, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest)
	return err
}

const keepBackups = 7

// dailyBackup keeps a fresh copy of the database in data/backups: whoever
// backs up the data directory with rsync or restic gets a consistent
// database with it, even if they copy the live one mid-write.
func (s *Server) dailyBackup(ctx context.Context) {
	dir := filepath.Join(s.cfg.DataDir, "backups")
	name := filepath.Join(dir, "trunk-"+time.Now().Format("2006-01-02")+".db")
	defer func() {
		// Every time, not only after a new copy: the oldest go also when today's existed.
		old, _ := filepath.Glob(filepath.Join(dir, "trunk-*.db"))
		sort.Strings(old)
		for len(old) > keepBackups {
			os.Remove(old[0])
			old = old[1:]
		}
	}()
	if _, err := os.Stat(name); err == nil {
		return // today's is done
	}
	tmp := name + ".tmp"
	os.Remove(tmp)
	if err := s.Backup(ctx, tmp); err != nil {
		s.log.Error("daily backup", "err", err)
		return
	}
	if err := os.Rename(tmp, name); err != nil {
		s.log.Error("daily backup", "err", err)
		return
	}
	s.log.Info("database backed up", "file", name)
}

// originalsRoot is the originals directory with a symlink resolved: kept on
// another disk, originals/ is often a link, which a directory walk would not
// enter.
func (s *Server) originalsRoot() string {
	if r, err := filepath.EvalSymlinks(s.originals); err == nil {
		return r
	}
	return s.originals
}

// --- verify ---

// Problem is something wrong with the library.
type Problem struct {
	Kind   string // missing, size, hash, unknown, thumbs
	Path   string // relative to originals
	Asset  int64
	Detail string
}

// VerifyOptions say how thorough Verify is.
type VerifyOptions struct {
	Hash   bool // read every file and compare its SHA-256 (slow: reads the whole library)
	Thumbs bool // render missing thumbnails
	// Progress is called now and then with files checked and the total.
	Progress func(done, total int)
}

// Verify checks that every file the database knows is there, as large as it
// should be and (with Hash) unchanged, and lists files in the originals that
// the database does not know. It changes nothing but missing thumbnails.
func (s *Server) Verify(ctx context.Context, opt VerifyOptions) ([]Problem, error) {
	type file struct {
		asset      int64
		path, sha  string
		size       int64
		kind, mime string
	}
	var files []file
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, sha256, size, kind, mime, orig_path, orig_sha256, orig_size,
		motion_path, motion_length FROM assets WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f file
		var op, osha, mp sql.NullString
		var osize, mlen sql.NullInt64
		if err := rows.Scan(&f.asset, &f.path, &f.sha, &f.size, &f.kind, &f.mime, &op, &osha, &osize, &mp, &mlen); err != nil {
			rows.Close()
			return nil, err
		}
		files = append(files, f)
		if op.Valid { // the first upload of an edited photo
			files = append(files, file{f.asset, op.String, osha.String, osize.Int64, f.kind, f.mime})
		}
		if mp.Valid { // a motion video kept apart; there is no hash to compare
			files = append(files, file{f.asset, mp.String, "", mlen.Int64, "video", "video/mp4"})
		}
	}
	rows.Close()

	var problems []Problem
	known := make(map[string]bool, len(files))
	for i, f := range files {
		if ctx.Err() != nil {
			return problems, ctx.Err()
		}
		if opt.Progress != nil && i%200 == 0 {
			opt.Progress(i, len(files))
		}
		known[f.path] = true
		abs := filepath.Join(s.originals, filepath.FromSlash(f.path))
		st, err := os.Stat(abs)
		switch {
		case err != nil:
			problems = append(problems, Problem{"missing", f.path, f.asset, err.Error()})
			continue
		case st.Size() != f.size:
			problems = append(problems, Problem{"size", f.path, f.asset, fmt.Sprintf("%d bytes, expected %d", st.Size(), f.size)})
			continue
		}
		if opt.Hash && f.sha != "" {
			sum, err := hashFile(abs)
			if err != nil {
				problems = append(problems, Problem{"missing", f.path, f.asset, err.Error()})
			} else if sum != f.sha {
				problems = append(problems, Problem{"hash", f.path, f.asset, "content changed or damaged"})
			}
		}
	}
	if opt.Thumbs {
		for _, f := range files {
			if ctx.Err() != nil {
				break
			}
			if !s.haveThumbs(f.sha) {
				a, err := s.scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id = ?`, f.asset))
				if err != nil || a.SHA256 != f.sha {
					continue // the first upload of an edit has no thumbnails of its own
				}
				if err := s.ensureThumbs(ctx, a); err != nil {
					problems = append(problems, Problem{"thumbs", f.path, f.asset, "thumbnails cannot be rendered"})
				}
			}
		}
	}
	if opt.Progress != nil {
		opt.Progress(len(files), len(files))
	}
	// Files nobody knows: left over from a crash, put there by hand, or from
	// a lost database (trunk reindex takes them in).
	root := s.originalsRoot()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !known[rel] && !strings.HasPrefix(d.Name(), ".") {
			problems = append(problems, Problem{Kind: "unknown", Path: rel, Detail: "not in the database"})
		}
		return nil
	})
	return problems, err
}

// --- reindex ---

// ReindexResult counts what Reindex did.
type ReindexResult struct {
	Added     int
	Duplicate int // same content as a known asset: left alone, reported
	Skipped   []string
}

// Reindex takes files in the originals that the database does not know into
// it, where they are: originals/<user>/… belongs to that user, who must
// exist. Photos come back with their date, place and camera; albums,
// captions, favourites and shares only live in the database, which is what
// the daily backups in data/backups are for.
func (s *Server) Reindex(ctx context.Context, progress func(path string)) (ReindexResult, error) {
	var res ReindexResult
	known := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM assets WHERE deleted_at IS NULL
		UNION SELECT orig_path FROM assets WHERE orig_path IS NOT NULL AND deleted_at IS NULL
		UNION SELECT motion_path FROM assets WHERE motion_path IS NOT NULL AND deleted_at IS NULL`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			known[p] = true
		}
	}
	rows.Close()
	users := map[string]int64{}
	rows, err = s.db.QueryContext(ctx, `SELECT id, name FROM users`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil {
			users[strings.ToLower(safeName(name, ""))] = id
		}
	}
	rows.Close()

	root := s.originalsRoot()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if known[rel] || strings.HasSuffix(rel, motionSuffix) {
			return nil // a motion video kept apart comes in with its photo
		}
		owner, _, _ := strings.Cut(rel, "/")
		uid, ok := users[strings.ToLower(owner)]
		if !ok {
			res.Skipped = append(res.Skipped, rel+" (no user "+owner+")")
			return nil
		}
		if progress != nil {
			progress(rel)
		}
		added, err := s.adopt(ctx, uid, rel)
		switch {
		case errors.Is(err, media.ErrUnsupported):
			res.Skipped = append(res.Skipped, rel+" (not a photo or video)")
		case err != nil:
			res.Skipped = append(res.Skipped, rel+" ("+err.Error()+")")
		case added:
			res.Added++
		default:
			res.Duplicate++
		}
		return nil
	})
	return res, err
}

// adopt records a file already in the originals as an asset of userID, where
// it is. A file with content the user already has is left out.
func (s *Server) adopt(ctx context.Context, userID int64, rel string) (bool, error) {
	abs := filepath.Join(s.originals, filepath.FromSlash(rel))
	sum, err := hashFile(abs)
	if err != nil {
		return false, err
	}
	if _, ok, err := s.findBySHA(ctx, userID, sum); err != nil || ok {
		return false, err
	}
	name := filepath.Base(abs)
	info, err := media.Extract(abs, name)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return false, err
	}
	taken := info.TakenAt
	if taken.IsZero() {
		taken = st.ModTime() // trunk set it to the capture time when it stored the file
	}
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	if _, ok, err := s.findBySHA(ctx, userID, sum); err != nil || ok {
		return false, err
	}
	id, err := s.insertAsset(ctx, ingestReq{UserID: userID, SHA256: sum, Name: name}, rel, info, st.Size(), taken, nil)
	if err != nil {
		return false, err
	}
	// Its motion video, if it was kept apart.
	if mp := motionPathFor(rel); info.MotionLength == 0 {
		if mst, err := os.Stat(filepath.Join(s.originals, filepath.FromSlash(mp))); err == nil {
			s.db.ExecContext(ctx, `UPDATE assets SET motion_path = ?, motion_offset = 0, motion_length = ? WHERE id = ?`, mp, mst.Size(), id)
		}
	}
	return true, nil
}
