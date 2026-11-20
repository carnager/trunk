package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Importer adds files to one user's library outside of HTTP, e.g. from a
// Google Takeout. It is safe to use from several goroutines, and alongside a
// running server on the same data directory.
type Importer struct {
	s        *Server
	userID   int64
	userName string
}

// Importer returns an importer for the named user.
func (s *Server) Importer(ctx context.Context, user string) (*Importer, error) {
	im := &Importer{s: s}
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM users WHERE name = ?`, user).Scan(&im.userID, &im.userName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no user %q", user)
	}
	return im, err
}

// Spooled is a file copied into the data directory, ready to be added
// without another copy.
type Spooled struct {
	path, sha256 string
}

// Spool copies r into the data directory, hashing it on the way. Hand the
// result to AddSpooled or EditSpooled, or Discard it.
func (im *Importer) Spool(r io.Reader) (Spooled, error) {
	tmp, sum, _, err := im.s.receiveAs("import-", r)
	return Spooled{tmp, sum}, err
}

// Discard removes a spooled file that will not be added.
func (sp Spooled) Discard() { os.Remove(sp.path) }

// Add stores the file read from r under its original name. Known content
// returns the existing asset with created=false.
func (im *Importer) Add(ctx context.Context, r io.Reader, name string, h *Hints) (a *Asset, created bool, err error) {
	sp, err := im.Spool(r)
	if err != nil {
		return nil, false, err
	}
	return im.AddSpooled(ctx, sp, name, h)
}

// AddSpooled is Add for a spooled file, which it takes over. An asset that is
// already there gets the hint's caption if it has none, so running an import
// again fills in what an older import left out.
func (im *Importer) AddSpooled(ctx context.Context, sp Spooled, name string, h *Hints) (a *Asset, created bool, err error) {
	a, created, err = im.s.ingest(ctx, ingestReq{
		UserID: im.userID, UserName: im.userName, TmpPath: sp.path, Name: name, SHA256: sp.sha256, Hints: h,
	})
	if err == nil && !created && h != nil && a.Caption == "" && strings.TrimSpace(h.Caption) != "" {
		if err := im.s.mutate(ctx, im.userID, []int64{a.ID}, `caption = ?`, strings.TrimSpace(h.Caption)); err != nil {
			return a, created, err
		}
		a, err = im.s.getAsset(ctx, im.userID, a.ID)
	}
	return a, created, err
}

// Edit makes the file read from r the current version of asset id, keeping
// the first upload. A file that is already another asset is left alone.
func (im *Importer) Edit(ctx context.Context, id int64, r io.Reader) error {
	sp, err := im.Spool(r)
	if err != nil {
		return err
	}
	return im.EditSpooled(ctx, id, sp)
}

// EditSpooled is Edit for a spooled file, which it takes over.
func (im *Importer) EditSpooled(ctx context.Context, id int64, sp Spooled) error {
	defer sp.Discard() // a no-op once the file is in the library
	a, err := im.s.getAsset(ctx, im.userID, id)
	if err != nil {
		return err
	}
	if a.Kind != "image" {
		return errNotPhoto
	}
	_, err = im.s.saveEdit(ctx, a, sp.path, sp.sha256)
	var conflict errEditConflict
	if errors.As(err, &conflict) {
		return nil
	}
	return err
}

// Album adds ids to the end of the album called name, creating it if needed.
// Ids already in it keep their place, so running an import again is harmless.
func (im *Importer) Album(ctx context.Context, name string, ids []int64) (created bool, err error) {
	id, ok, err := im.s.AlbumByName(ctx, im.userID, name)
	if err != nil {
		return false, err
	}
	if !ok {
		if id, err = im.s.CreateAlbum(ctx, im.userID, name); err != nil {
			return false, err
		}
	}
	if len(ids) == 0 {
		return !ok, nil
	}
	return !ok, im.s.AddToAlbum(ctx, im.userID, id, ids, true)
}

// Thumbs renders the thumbnails of asset id now, so the library is quick to
// browse right after an import.
func (im *Importer) Thumbs(ctx context.Context, id int64) error {
	a, err := im.s.getAsset(ctx, im.userID, id)
	if err != nil {
		return err
	}
	return im.s.ensureThumbs(ctx, a)
}
