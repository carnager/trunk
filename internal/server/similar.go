package server

import (
	"cmp"
	"context"
	"database/sql"
	"net/http"
	"slices"
	"time"

	"github.com/carnager/trunk/internal/media"
)

// The look-alike finder. Byte-identical uploads are caught on the way in;
// this finds what is the same picture in another file (a WhatsApp copy, an
// export, another size) and bursts of nearly identical shots, so a library
// can be cleaned up. It only suggests; deleting is up to the user.

// Thresholds on the fingerprint distance (bits out of 512), measured on a
// real library: copies of a photo (exports, downloads, edits saved as
// "~2") lie well below sameDistance, unrelated pictures rarely come near it.
var (
	sameDistance  = 40 // the same picture, taken within sameWindow
	farDistance   = 24 // the same picture, taken further apart (a download, an export)
	sameWindow    = 48 * time.Hour
	burstDistance = 90              // nearly the same, if taken close together
	burstWindow   = 5 * time.Minute // "close together"
	// Below this much detail (see media.Fingerprint) a picture only counts
	// within a burst: night skies, walls and pocket shots all look alike.
	minDetail = 4.0
)

// fingerprint computes and stores the fingerprint of a's content, from its
// small thumbnail. Done once per content.
func (s *Server) fingerprint(ctx context.Context, a *Asset) error {
	var have int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fingerprints WHERE sha256 = ?`, a.SHA256).Scan(&have)
	if have > 0 {
		return nil
	}
	if err := s.ensureThumbs(ctx, a); err != nil {
		return err
	}
	h, detail, err := media.Fingerprint(s.thumbPath(a.SHA256, media.Small))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO fingerprints (sha256, hash, detail) VALUES (?, ?, ?)`, a.SHA256, h.Bytes(), detail)
	return err
}

// fingerprinter fills in missing fingerprints at start and then hourly.
func (s *Server) fingerprinter(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.fillFingerprints(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fillFingerprints fingerprints everything that has none yet: the library
// from before trunk had them, once, and whatever slipped past since.
func (s *Server) fillFingerprints(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE deleted_at IS NULL AND sha256 NOT IN (SELECT sha256 FROM fingerprints)`)
	if err != nil {
		s.log.Error("fingerprints", "err", err)
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
	done := 0
	for _, a := range todo {
		if ctx.Err() != nil {
			return
		}
		if err := s.fingerprint(ctx, a); err == nil {
			done++
		}
	}
	s.log.Info("fingerprints computed", "assets", done, "of", len(todo), "took", time.Since(start).Round(time.Second))
}

// SimilarGroup is a set of the user's photos that look alike.
type SimilarGroup struct {
	AssetIDs []int64 `json:"asset_ids"` // newest first
	Keep     int64   `json:"keep"`      // the suggestion: the one to keep
	// "same": the same picture in different files; "burst": nearly identical
	// shots taken close together; "motion": a video that is a copy of the
	// video inside a motion photo (Takeout exports it apart too).
	Kind string `json:"kind"`
}

type fingerprinted struct {
	a      *Asset
	hash   media.Hash
	detail float64
}

// handleSimilar: GET /api/v1/similar → {"groups": [...], "pending": n}.
// pending counts photos not fingerprinted yet, which cannot be matched.
func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := sessionFrom(r).UserID
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed("a.", assetCols)+`, f.hash, f.detail FROM assets a
		LEFT JOIN fingerprints f ON f.sha256 = a.sha256
		WHERE a.user_id = ? AND a.deleted_at IS NULL AND a.trashed_at IS NULL`, me)
	if err != nil {
		s.internal(w, err)
		return
	}
	var list []fingerprinted
	pending := 0
	for rows.Next() {
		var raw []byte
		var detail sql.NullFloat64
		a, err := s.scanAsset(scanWith(rows, &raw, &detail))
		if err != nil {
			rows.Close()
			s.internal(w, err)
			return
		}
		h, ok := media.HashFrom(raw)
		if !ok {
			pending++
			continue
		}
		list = append(list, fingerprinted{a, h, detail.Float64})
	}
	rows.Close()

	ignored := map[[2]int64]bool{}
	irows, err := s.db.QueryContext(ctx, `SELECT a, b FROM similar_ignored WHERE user_id = ?`, me)
	if err != nil {
		s.internal(w, err)
		return
	}
	for irows.Next() {
		var p [2]int64
		if irows.Scan(&p[0], &p[1]) == nil {
			ignored[p] = true
		}
	}
	irows.Close()

	groups := similarGroups(list, ignored)
	motion, err := s.motionCopies(ctx, me, ignored)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": append(motion, groups...), "pending": pending})
}

// lookAlike says whether two photos belong in one group, and whether as a
// burst (shots moments apart) or as the same picture (a copy).
func lookAlike(a, b fingerprinted, ignored map[[2]int64]bool) (ok, burst bool) {
	if a.a.Kind != b.a.Kind {
		return false, false
	}
	d := media.Distance(a.hash, b.hash)
	if d > burstDistance || ignored[pair(a.a.ID, b.a.ID)] {
		return false, false
	}
	// Shots taken moments apart are a burst, however alike. The same picture
	// is a copy: the same moment (its EXIF came along) or any other time (a
	// download, an export). An even, featureless picture (a pocket shot, a
	// black screen) has a fingerprint of almost no set bits, close to every
	// other such picture: those only count within a burst.
	if closeInTime(a.a, b.a) && taken(a.a) != taken(b.a) {
		return true, true
	}
	limit := farDistance
	if a.a.TakenAt != nil && b.a.TakenAt != nil && a.a.TakenAt.Sub(*b.a.TakenAt).Abs() <= sameWindow {
		limit = sameDistance
	}
	if d <= limit && a.detail >= minDetail && b.detail >= minDetail {
		return true, false
	}
	return false, false
}

// similarGroups groups photos that look alike, so that a burst of ten shots
// is one group rather than 45 pairs. Every photo in a group looks like every
// other one (complete linkage): A like B and B like C does not put A with C.
func similarGroups(list []fingerprinted, ignored map[[2]int64]bool) []SimilarGroup {
	type edge struct{ i, j, d int }
	var edges []edge
	for i := range list {
		for j := i + 1; j < len(list); j++ {
			if ok, _ := lookAlike(list[i], list[j], ignored); ok {
				edges = append(edges, edge{i, j, media.Distance(list[i].hash, list[j].hash)})
			}
		}
	}
	slices.SortFunc(edges, func(x, y edge) int { return cmp.Compare(x.d, y.d) })
	group := make([]int, len(list)) // index of each photo's group in members
	members := make([][]int, len(list))
	for i := range list {
		group[i] = i
		members[i] = []int{i}
	}
	for _, e := range edges {
		gi, gj := group[e.i], group[e.j]
		if gi == gj {
			continue
		}
		all := true
		for _, x := range members[gi] {
			for _, y := range members[gj] {
				if ok, _ := lookAlike(list[x], list[y], ignored); !ok {
					all = false
				}
			}
		}
		if !all {
			continue
		}
		for _, y := range members[gj] {
			group[y] = gi
		}
		members[gi] = append(members[gi], members[gj]...)
		members[gj] = nil
	}

	var groups []SimilarGroup
	newest := map[int64]int64{}
	for _, m := range members {
		if len(m) < 2 {
			continue
		}
		as := make([]*Asset, len(m))
		burst := true
		for k, i := range m {
			as[k] = list[i].a
			for _, j := range m[k+1:] {
				if _, b := lookAlike(list[i], list[j], ignored); !b {
					burst = false
				}
			}
		}
		slices.SortFunc(as, func(x, y *Asset) int { return cmp.Compare(taken(y), taken(x)) })
		g := SimilarGroup{Kind: "same", Keep: best(as).ID}
		if burst {
			g.Kind = "burst"
		}
		for _, a := range as {
			g.AssetIDs = append(g.AssetIDs, a.ID)
		}
		newest[as[0].ID] = taken(as[0])
		groups = append(groups, g)
	}
	// Newest first, like the library.
	slices.SortFunc(groups, func(x, y SimilarGroup) int { return cmp.Compare(newest[y.AssetIDs[0]], newest[x.AssetIDs[0]]) })
	return groups
}

func taken(a *Asset) int64 {
	if a.TakenAt == nil {
		return 0
	}
	return a.TakenAt.UnixMilli()
}

func closeInTime(a, b *Asset) bool {
	if a.TakenAt == nil || b.TakenAt == nil {
		return false
	}
	d := a.TakenAt.Sub(*b.TakenAt)
	return d < burstWindow && d > -burstWindow
}

func pair(a, b int64) [2]int64 {
	if a > b {
		a, b = b, a
	}
	return [2]int64{a, b}
}

// best is the one to keep: the most pixels, then what the user put work into
// (an edit, a favourite, a caption), then the larger file (less compressed),
// then the earliest upload.
func best(as []*Asset) *Asset {
	score := func(a *Asset) []int64 {
		b := func(v bool) int64 {
			if v {
				return 1
			}
			return 0
		}
		return []int64{int64(a.Width) * int64(a.Height), b(a.EditedAt != nil), b(a.Favorite), b(a.Caption != ""), a.Size, -a.ID}
	}
	return slices.MaxFunc(as, func(x, y *Asset) int { return slices.Compare(score(x), score(y)) })
}

// handleSimilarIgnore: POST /api/v1/similar/ignore {"ids": [..]}: these do
// not count as look-alikes of each other any more.
func (s *Server) handleSimilarIgnore(w http.ResponseWriter, r *http.Request) {
	ids, ok := readIDs(w, r)
	if !ok {
		return
	}
	me := sessionFrom(r).UserID
	err := s.inTx(r.Context(), func(tx *sql.Tx, _, _ int64) error {
		for i, a := range ids {
			for _, b := range ids[i+1:] {
				p := pair(a, b)
				if _, err := tx.ExecContext(r.Context(), `INSERT OR IGNORE INTO similar_ignored (user_id, a, b) VALUES (?, ?, ?)`, me, p[0], p[1]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// motionCopies finds the user's videos that are byte for byte the video of
// one of their motion photos: the photo is the one to keep.
func (s *Server) motionCopies(ctx context.Context, userID int64, ignored map[[2]int64]bool) ([]SimilarGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, v.id FROM assets v
		JOIN assets i ON i.user_id = v.user_id AND i.motion_sha256 = v.sha256 AND i.id != v.id
		WHERE v.user_id = ? AND v.kind = 'video'
			AND v.deleted_at IS NULL AND v.trashed_at IS NULL AND i.deleted_at IS NULL AND i.trashed_at IS NULL
		ORDER BY i.taken_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []SimilarGroup
	seen := map[int64]bool{}
	for rows.Next() {
		var photo, video int64
		if err := rows.Scan(&photo, &video); err != nil {
			return nil, err
		}
		if seen[video] || ignored[pair(photo, video)] {
			continue
		}
		seen[video] = true
		groups = append(groups, SimilarGroup{AssetIDs: []int64{photo, video}, Keep: photo, Kind: "motion"})
	}
	return groups, rows.Err()
}
