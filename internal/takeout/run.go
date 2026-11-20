package takeout

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/carnager/trunk/internal/server"
)

// Result counts what an import did.
type Result struct {
	Added, Existing, Edits, Failed int
	AlbumsCreated, AlbumsUpdated   int
	Errors                         []string
}

// Run imports the plan into im with jobs files in flight at once, printing
// progress to log every few seconds.
func Run(ctx context.Context, im *server.Importer, p *Plan, jobs int, log io.Writer) Result {
	var res Result
	var mu sync.Mutex
	fail := func(it *Item, err error) {
		mu.Lock()
		res.Failed++
		res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", it.Path, err))
		mu.Unlock()
	}

	ids := make(map[*Item]int64, len(p.Items))
	taken := make(map[int64]time.Time, len(p.Items))
	var done atomic.Int64
	total := len(p.Items)

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				mu.Lock()
				fmt.Fprintf(log, "  %d/%d: %d added, %d already there, %d edits, %d failed\n",
					done.Load(), total, res.Added, res.Existing, res.Edits, res.Failed)
				mu.Unlock()
			}
		}
	}()

	// job is an item to import, with its content already copied into the data
	// directory if it came out of a tar archive.
	type job struct {
		it *Item
		sp *server.Spooled
	}
	work := func(feed func(chan<- job), do func(job) error) {
		ch := make(chan job)
		var wg sync.WaitGroup
		for range max(1, jobs) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					if err := do(j); err != nil {
						fail(j.it, err)
					}
					done.Add(1)
				}
			}()
		}
		feed(ch)
		close(ch)
		wg.Wait()
	}

	// Items inside tar archives, by archive and path; archives in input order.
	var archives []*tarArchive
	inArchive := map[*tarArchive]map[string]*Item{}
	var originals, edits []*Item
	for _, it := range p.Items {
		if it.EditOf != nil {
			edits = append(edits, it)
		} else {
			originals = append(originals, it)
		}
		if a := it.archive; a != nil {
			if inArchive[a] == nil {
				inArchive[a] = map[string]*Item{}
				archives = append(archives, a)
			}
			inArchive[a][it.Path] = it
		}
	}
	spooledEdits := map[*Item]server.Spooled{}

	work(func(ch chan<- job) {
		for _, it := range originals {
			if it.archive == nil && ctx.Err() == nil {
				ch <- job{it: it}
			}
		}
		// Tar archives are read once, front to back. Each wanted file is copied
		// into the data directory (where adding it is a rename, not a copy) and
		// handed to the workers, which hash, inspect and thumbnail in parallel.
		// Edits can come before their original, so they wait for the end.
		for _, a := range archives {
			err := a.walk(func(h *tar.Header, r io.Reader) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				it := inArchive[a][h.Name]
				if it == nil {
					return nil
				}
				sp, err := im.Spool(r)
				if err != nil {
					fail(it, err)
					return nil
				}
				if it.EditOf != nil {
					mu.Lock()
					spooledEdits[it] = sp
					mu.Unlock()
					return nil
				}
				ch <- job{it: it, sp: &sp}
				return nil
			})
			if err != nil && ctx.Err() == nil {
				mu.Lock()
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", a.path, err))
				mu.Unlock()
			}
		}
	}, func(j job) error {
		var a *server.Asset
		var created bool
		var err error
		if j.sp != nil {
			a, created, err = im.AddSpooled(ctx, *j.sp, j.it.Name, hints(j.it.Sidecar))
		} else {
			var r io.ReadCloser
			if r, err = j.it.Open(); err != nil {
				return err
			}
			a, created, err = im.Add(ctx, r, j.it.Name, hints(j.it.Sidecar))
			r.Close()
		}
		if err != nil {
			return err
		}
		mu.Lock()
		ids[j.it] = a.ID
		if a.TakenAt != nil {
			taken[a.ID] = *a.TakenAt
		}
		if created {
			res.Added++
		} else {
			res.Existing++
		}
		mu.Unlock()
		if created {
			im.Thumbs(ctx, a.ID) // a failure here is not an import failure; the server retries on demand
		}
		return nil
	})

	// Edits after all originals, so each finds its asset.
	work(func(ch chan<- job) {
		for _, it := range edits {
			if ctx.Err() != nil {
				break
			}
			ch <- job{it: it}
		}
	}, func(j job) error {
		mu.Lock()
		id, ok := ids[j.it.EditOf]
		sp, spooled := spooledEdits[j.it]
		delete(spooledEdits, j.it)
		mu.Unlock()
		if !ok {
			if spooled {
				sp.Discard()
			}
			return fmt.Errorf("original %s was not imported", j.it.EditOf.Name)
		}
		var err error
		switch {
		case spooled:
			err = im.EditSpooled(ctx, id, sp)
		case j.it.Open != nil:
			var r io.ReadCloser
			if r, err = j.it.Open(); err != nil {
				return err
			}
			err = im.Edit(ctx, id, r)
			r.Close()
		default:
			err = fmt.Errorf("not read from its archive")
		}
		if err != nil {
			return err
		}
		im.Thumbs(ctx, id)
		mu.Lock()
		res.Edits++
		mu.Unlock()
		return nil
	})
	close(stop)
	for _, sp := range spooledEdits {
		sp.Discard() // interrupted before their turn
	}

	for _, al := range p.Albums {
		if ctx.Err() != nil {
			break
		}
		var list []int64
		seen := map[int64]bool{}
		for _, it := range al.Items {
			if id, ok := ids[it]; ok && !seen[id] {
				seen[id] = true
				list = append(list, id)
			}
		}
		// Google shows albums oldest first.
		slices.SortStableFunc(list, func(a, b int64) int { return taken[a].Compare(taken[b]) })
		created, err := im.Album(ctx, al.Name, list)
		switch {
		case err != nil:
			res.Errors = append(res.Errors, fmt.Sprintf("album %q: %v", al.Name, err))
		case created:
			res.AlbumsCreated++
		default:
			res.AlbumsUpdated++
		}
	}
	return res
}

func hints(sc *Sidecar) *server.Hints {
	if sc == nil {
		return nil
	}
	h := &server.Hints{TakenAt: sc.Taken(), Favorite: sc.Favorited, Caption: sc.Description}
	if lat, lon, alt, ok := sc.Location(); ok {
		h.Lat, h.Lon = &lat, &lon
		if alt != 0 {
			h.Alt = &alt
		}
	}
	return h
}
