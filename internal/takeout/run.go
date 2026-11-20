package takeout

import (
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

	work := func(items []*Item, do func(*Item) error) {
		ch := make(chan *Item)
		var wg sync.WaitGroup
		for range max(1, jobs) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for it := range ch {
					if err := do(it); err != nil {
						fail(it, err)
					}
					done.Add(1)
				}
			}()
		}
		for _, it := range items {
			if ctx.Err() != nil {
				break
			}
			ch <- it
		}
		close(ch)
		wg.Wait()
	}

	var originals, edits []*Item
	for _, it := range p.Items {
		if it.EditOf != nil {
			edits = append(edits, it)
		} else {
			originals = append(originals, it)
		}
	}

	work(originals, func(it *Item) error {
		r, err := it.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		a, created, err := im.Add(ctx, r, it.Name, hints(it.Sidecar))
		if err != nil {
			return err
		}
		mu.Lock()
		ids[it] = a.ID
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
	work(edits, func(it *Item) error {
		mu.Lock()
		id, ok := ids[it.EditOf]
		mu.Unlock()
		if !ok {
			return fmt.Errorf("original %s was not imported", it.EditOf.Name)
		}
		r, err := it.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		if err := im.Edit(ctx, id, r); err != nil {
			return err
		}
		im.Thumbs(ctx, id)
		mu.Lock()
		res.Edits++
		mu.Unlock()
		return nil
	})
	close(stop)

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
	h := &server.Hints{TakenAt: sc.Taken(), Favorite: sc.Favorited}
	if lat, lon, alt, ok := sc.Location(); ok {
		h.Lat, h.Lon = &lat, &lon
		if alt != 0 {
			h.Alt = &alt
		}
	}
	return h
}
