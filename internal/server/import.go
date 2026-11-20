package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
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

// Add stores the file read from r under its original name. Known content
// returns the existing asset with created=false.
func (im *Importer) Add(ctx context.Context, r io.Reader, name string, h *Hints) (a *Asset, created bool, err error) {
	tmp, sum, _, err := im.s.receive(r)
	if err != nil {
		return nil, false, err
	}
	return im.s.ingest(ctx, ingestReq{
		UserID: im.userID, UserName: im.userName, TmpPath: tmp, Name: name, SHA256: sum, Hints: h,
	})
}

// Edit makes the file read from r the current version of asset id, keeping
// the first upload. A file that is already another asset is left alone.
func (im *Importer) Edit(ctx context.Context, id int64, r io.Reader) error {
	a, err := im.s.getAsset(ctx, im.userID, id)
	if err != nil {
		return err
	}
	if a.Kind != "image" {
		return errNotPhoto
	}
	tmp, sum, _, err := im.s.receive(r)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, err = im.s.saveEdit(ctx, a, tmp, sum)
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
