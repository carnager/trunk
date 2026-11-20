package server

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"
)

// People. trunk-ai finds faces and sends a vector per face (SFace, 128
// numbers); trunk groups them into people per user and keeps the groups
// stable as photos arrive: a group keeps the person most of its faces had,
// names stick, merges and rejections the user made are kept.

const (
	facesModel = "faces-yunet-sface-v1"
	// Grouping (DBSCAN): faces this alike are neighbours; a frontal face
	// with this many frontal neighbours (itself included) starts or extends
	// a person. Tuned on a family library: adults group well from 0.55,
	// children only stay apart from 0.65, and a group that mixes two people
	// is worse than two groups of one (merging is a tap).
	faceEps    = 0.65
	faceMinPts = 3
)

// handleFacesTodo: GET /api/v1/ai/faces/todo?model=M[&limit=N] (admin) →
// photos not yet looked at for faces: {"assets": [{"id", "sha256"}], "remaining"}.
func (s *Server) handleFacesTodo(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	model, ok := modelParam(w, r.URL.Query().Get("model"))
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	const missing = `FROM assets a WHERE a.deleted_at IS NULL AND a.kind = 'image'
		AND NOT EXISTS (SELECT 1 FROM face_scans f WHERE f.sha256 = a.sha256 AND f.model = ?)`
	var remaining int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(DISTINCT a.sha256) `+missing, model).Scan(&remaining); err != nil {
		s.internal(w, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT MIN(a.id), a.sha256 `+missing+` GROUP BY a.sha256 ORDER BY MIN(a.id) DESC LIMIT ?`, model, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	type todo struct {
		ID     int64  `json:"id"`
		SHA256 string `json:"sha256"`
	}
	out := []todo{}
	for rows.Next() {
		var t todo
		if rows.Scan(&t.ID, &t.SHA256) == nil {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": out, "remaining": remaining})
}

// handleFacesPut: PUT /api/v1/ai/faces (admin) {"model", "items": [{"sha256",
// "faces": [{"box": [x, y, w, h] (0..1), "score", "frontal", "vec": base64}]}]}.
// A file sent again replaces what was found in it before.
func (s *Server) handleFacesPut(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	var req struct {
		Model string `json:"model"`
		Items []struct {
			SHA256 string `json:"sha256"`
			Faces  []struct {
				Box     [4]float64 `json:"box"`
				Score   float64    `json:"score"`
				Frontal bool       `json:"frontal"`
				Vec     string     `json:"vec"`
			} `json:"faces"`
		} `json:"items"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	model, ok := modelParam(w, req.Model)
	if !ok {
		return
	}
	if len(req.Items) > 1000 {
		httpError(w, http.StatusBadRequest, "at most 1000 items")
		return
	}
	err := s.inTx(r.Context(), func(tx *sql.Tx, _, _ int64) error {
		for _, it := range req.Items {
			if len(it.SHA256) != 64 {
				return errBadRequest("bad sha256")
			}
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM faces WHERE sha256 = ? AND model = ?`, it.SHA256, model); err != nil {
				return err
			}
			for _, f := range it.Faces {
				raw, err := base64.StdEncoding.DecodeString(f.Vec)
				if err != nil || len(raw) == 0 || len(raw)%4 != 0 || len(raw) > 4096 {
					return errBadRequest("bad face vector")
				}
				if _, err := tx.ExecContext(r.Context(), `INSERT INTO faces (sha256, model, x, y, w, h, score, frontal, vec) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					it.SHA256, model, f.Box[0], f.Box[1], f.Box[2], f.Box[3], f.Score, boolInt(f.Frontal), raw); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(r.Context(), `INSERT OR IGNORE INTO face_scans (sha256, model) VALUES (?, ?)`, it.SHA256, model); err != nil {
				return err
			}
		}
		return nil
	})
	var bad errBadRequest
	if errors.As(err, &bad) {
		httpError(w, http.StatusBadRequest, string(bad))
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.regroupSoon()
	writeJSON(w, http.StatusOK, map[string]int{"stored": len(req.Items)})
}

type errBadRequest string

func (e errBadRequest) Error() string { return string(e) }

// regroupSoon asks the grouper to run once the indexer pauses.
func (s *Server) regroupSoon() {
	select {
	case s.regroupQ <- struct{}{}:
	default:
	}
}

// grouper regroups faces into people a little after new faces arrive, and
// once at start.
func (s *Server) grouper(ctx context.Context) {
	s.regroupAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.regroupQ:
		}
		// trunk-ai sends batch after batch: wait for a pause.
		for quiet := false; !quiet; {
			select {
			case <-ctx.Done():
				return
			case <-s.regroupQ:
			case <-time.After(20 * time.Second):
				quiet = true
			}
		}
		s.regroupAll(ctx)
	}
}

func (s *Server) regroupAll(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM users`)
	if err != nil {
		return
	}
	var users []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			users = append(users, id)
		}
	}
	rows.Close()
	for _, u := range users {
		if err := s.regroup(ctx, u); err != nil && ctx.Err() == nil {
			s.log.Error("grouping faces", "user", u, "err", err)
		}
	}
}

type userFace struct {
	id      int64
	vec     []float32
	frontal bool
	person  int64 // current, 0 for none
	manual  bool
	reject  int64
}

// regroup groups one user's faces into people, keeping what is known.
func (s *Server) regroup(ctx context.Context, userID int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT f.id, f.vec, f.frontal,
			COALESCE(fp.person_id, 0), COALESCE(fp.manual, 0), COALESCE(fp.rejected, 0)
		FROM faces f
		JOIN assets a ON a.sha256 = f.sha256 AND a.user_id = ? AND a.deleted_at IS NULL AND a.trashed_at IS NULL
		LEFT JOIN face_people fp ON fp.face_id = f.id AND fp.user_id = ?
		WHERE f.model = ?`, userID, userID, facesModel)
	if err != nil {
		return err
	}
	var faces []userFace
	for rows.Next() {
		var f userFace
		var raw []byte
		var frontal, manual int
		if err := rows.Scan(&f.id, &raw, &frontal, &f.person, &manual, &f.reject); err != nil {
			rows.Close()
			return err
		}
		f.vec, f.frontal, f.manual = decodeVec(raw), frontal == 1, manual == 1
		faces = append(faces, f)
	}
	rows.Close()
	if len(faces) == 0 {
		return nil
	}
	vecs := make([][]float32, len(faces))
	frontal := make([]bool, len(faces))
	for i, f := range faces {
		vecs[i], frontal[i] = f.vec, f.frontal
	}
	labels := dbscan(vecs, frontal, faceEps, faceMinPts)

	start := time.Now()
	return s.inTx(ctx, func(tx *sql.Tx, _, now int64) error {
		groups := map[int][]int{}
		for i, l := range labels {
			if l >= 0 {
				groups[l] = append(groups[l], i)
			}
		}
		target := make([]int64, len(faces)) // the person each face should have; 0 none
		for _, members := range groups {
			// The person most of these faces already belong to keeps them.
			votes := map[int64]int{}
			for _, i := range members {
				if p := faces[i].person; p != 0 {
					votes[p]++
				}
			}
			var person int64
			best := 0
			for p, n := range votes {
				if n > best || (n == best && p < person) {
					person, best = p, n
				}
			}
			if person == 0 {
				res, err := tx.ExecContext(ctx, `INSERT INTO people (user_id, created_at) VALUES (?, ?)`, userID, now)
				if err != nil {
					return err
				}
				person, _ = res.LastInsertId()
			}
			for _, i := range members {
				target[i] = person
			}
		}
		for i, f := range faces {
			if f.manual {
				continue // the user decided
			}
			t := target[i]
			if t == f.reject {
				t = 0
			}
			if t == f.person {
				continue
			}
			var p any
			if t != 0 {
				p = t
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO face_people (user_id, face_id, person_id, rejected) VALUES (?, ?, ?, ?)
				ON CONFLICT (user_id, face_id) DO UPDATE SET person_id = excluded.person_id`, userID, f.id, p, nilIfZero(f.reject)); err != nil {
				return err
			}
		}
		// People nobody named who lost their faces go.
		_, err := tx.ExecContext(ctx, `DELETE FROM people WHERE user_id = ? AND name = ''
			AND NOT EXISTS (SELECT 1 FROM face_people fp WHERE fp.person_id = people.id)`, userID)
		if err == nil {
			s.log.Debug("faces grouped", "user", userID, "faces", len(faces), "groups", len(groups), "took", time.Since(start).Round(time.Millisecond))
		}
		return err
	})
}

func nilIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// dbscan: a frontal face with at least minPts frontal neighbours (similarity
// ≥ eps, itself included) is a core; cores near each other form a group, and
// other faces near a core join it. Profiles never start a group: they are
// recognised too poorly. The rest is -1.
func dbscan(vecs [][]float32, frontal []bool, eps float64, minPts int) []int {
	n := len(vecs)
	near := make([][]int32, n)
	for i := 0; i < n; i++ {
		a := vecs[i]
		for j := i + 1; j < n; j++ {
			b := vecs[j]
			if len(b) != len(a) {
				continue
			}
			var dot float32
			for k := range a {
				dot += a[k] * b[k]
			}
			if float64(dot) >= eps {
				near[i] = append(near[i], int32(j))
				near[j] = append(near[j], int32(i))
			}
		}
	}
	core := make([]bool, n)
	for i := range core {
		if !frontal[i] {
			continue
		}
		c := 1
		for _, j := range near[i] {
			if frontal[j] {
				c++
			}
		}
		core[i] = c >= minPts
	}
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	next := 0
	for i := 0; i < n; i++ {
		if !core[i] || labels[i] >= 0 {
			continue
		}
		labels[i] = next
		queue := []int{i}
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			for _, q32 := range near[p] {
				q := int(q32)
				if labels[q] >= 0 {
					continue
				}
				labels[q] = next
				if core[q] {
					queue = append(queue, q)
				}
			}
		}
		next++
	}
	return labels
}

// --- people for clients ---

// Person is one of a user's people.
type Person struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Hidden   bool    `json:"hidden,omitempty"`
	Faces    int     `json:"faces"`
	Cover    int64   `json:"cover"` // a face: GET /faces/{id}/thumb
	AssetIDs []int64 `json:"asset_ids"`
}

// people lists userID's people with their photos, most photographed first.
func (s *Server) people(ctx context.Context, userID int64) ([]*Person, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.name, p.hidden, f.id, f.w * f.h * f.score, f.frontal, a.id, a.taken_at
		FROM people p
		JOIN face_people fp ON fp.person_id = p.id AND fp.user_id = p.user_id
		JOIN faces f ON f.id = fp.face_id
		JOIN assets a ON a.sha256 = f.sha256 AND a.user_id = p.user_id AND a.deleted_at IS NULL AND a.trashed_at IS NULL
		WHERE p.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*Person{}
	best := map[int64]float64{}
	taken := map[int64]map[int64]int64{} // person → asset → taken
	faces := map[int64]map[int64]bool{}
	for rows.Next() {
		var pid, fid, aid, t int64
		var name string
		var hidden, frontal int
		var q float64
		if err := rows.Scan(&pid, &name, &hidden, &fid, &q, &frontal, &aid, &t); err != nil {
			return nil, err
		}
		p := byID[pid]
		if p == nil {
			p = &Person{ID: pid, Name: name, Hidden: hidden == 1}
			byID[pid] = p
			taken[pid] = map[int64]int64{}
			faces[pid] = map[int64]bool{}
		}
		faces[pid][fid] = true
		taken[pid][aid] = t
		if frontal == 1 {
			q *= 2 // a face looking at the camera makes the better picture
		}
		if q > best[pid] {
			best[pid], p.Cover = q, fid
		}
	}
	out := make([]*Person, 0, len(byID))
	for id, p := range byID {
		p.Faces = len(faces[id])
		for aid := range taken[id] {
			p.AssetIDs = append(p.AssetIDs, aid)
		}
		slices.SortFunc(p.AssetIDs, func(a, b int64) int { return cmp.Compare(taken[id][b], taken[id][a]) })
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b *Person) int {
		if c := cmp.Compare(len(b.AssetIDs), len(a.AssetIDs)); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, rows.Err()
}

// handlePeople: GET /api/v1/people → {"people": [...]}.
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	list, err := s.people(r.Context(), sessionFrom(r).UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": list})
}

func (s *Server) ownPerson(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	var n int
	s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM people WHERE id = ? AND user_id = ?`, id, sessionFrom(r).UserID).Scan(&n)
	if n == 0 {
		httpError(w, http.StatusNotFound, "no such person")
		return 0, false
	}
	return id, true
}

// handlePatchPerson: PATCH /api/v1/people/{id} {"name", "hidden"}.
func (s *Server) handlePatchPerson(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownPerson(w, r)
	if !ok {
		return
	}
	var req struct {
		Name   *string `json:"name"`
		Hidden *bool   `json:"hidden"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if len(name) > 200 {
			httpError(w, http.StatusBadRequest, "name too long")
			return
		}
		if _, err := s.db.ExecContext(r.Context(), `UPDATE people SET name = ? WHERE id = ?`, name, id); err != nil {
			s.internal(w, err)
			return
		}
	}
	if req.Hidden != nil {
		if _, err := s.db.ExecContext(r.Context(), `UPDATE people SET hidden = ? WHERE id = ?`, boolInt(*req.Hidden), id); err != nil {
			s.internal(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMergePeople: POST /api/v1/people/{id}/merge {"ids": [..]}: those
// people are this one. Their faces move here for good.
func (s *Server) handleMergePeople(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownPerson(w, r)
	if !ok {
		return
	}
	ids, ok := readIDs(w, r)
	if !ok {
		return
	}
	me := sessionFrom(r).UserID
	err := s.inTx(r.Context(), func(tx *sql.Tx, _, _ int64) error {
		for _, other := range ids {
			if other == id {
				continue
			}
			var name string
			if err := tx.QueryRowContext(r.Context(), `SELECT name FROM people WHERE id = ? AND user_id = ?`, other, me).Scan(&name); err != nil {
				return errBadRequest("not your person")
			}
			// The merged person's name is kept if this one has none.
			if name != "" {
				tx.ExecContext(r.Context(), `UPDATE people SET name = ? WHERE id = ? AND name = ''`, name, id)
			}
			if _, err := tx.ExecContext(r.Context(), `UPDATE face_people SET person_id = ?, manual = 1 WHERE person_id = ? AND user_id = ?`, id, other, me); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), `UPDATE face_people SET rejected = NULL WHERE rejected = ? AND user_id = ?`, other, me); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM people WHERE id = ?`, other); err != nil {
				return err
			}
		}
		// Faces regrouping put with this person are the user's choice now too.
		_, err := tx.ExecContext(r.Context(), `UPDATE face_people SET manual = 1 WHERE person_id = ? AND user_id = ?`, id, me)
		return err
	})
	var bad errBadRequest
	if errors.As(err, &bad) {
		httpError(w, http.StatusBadRequest, string(bad))
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRejectFace: DELETE /api/v1/people/{id}/faces/{face}: not this
// person; the face does not go back to them.
func (s *Server) handleRejectFace(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownPerson(w, r)
	if !ok {
		return
	}
	face, err := strconv.ParseInt(r.PathValue("face"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE face_people SET person_id = NULL, manual = 0, rejected = ?
		WHERE face_id = ? AND person_id = ? AND user_id = ?`, id, face, id, sessionFrom(r).UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "not a face of this person")
		return
	}
	s.db.ExecContext(r.Context(), `DELETE FROM people WHERE id = ? AND name = ''
		AND NOT EXISTS (SELECT 1 FROM face_people WHERE person_id = ?)`, id, id)
	w.WriteHeader(http.StatusNoContent)
}

// FaceRef is a face in a photo.
type FaceRef struct {
	ID     int64      `json:"id"`
	Box    [4]float64 `json:"box"` // x, y, w, h relative to the picture
	Person int64      `json:"person,omitempty"`
	Name   string     `json:"name,omitempty"`
}

// handleAssetFaces: GET /api/v1/assets/{id}/faces → {"faces": [...]}.
func (s *Server) handleAssetFaces(w http.ResponseWriter, r *http.Request) {
	a, ok := s.assetFromPath(w, r)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT f.id, f.x, f.y, f.w, f.h, COALESCE(fp.person_id, 0), COALESCE(p.name, '')
		FROM faces f
		LEFT JOIN face_people fp ON fp.face_id = f.id AND fp.user_id = ?
		LEFT JOIN people p ON p.id = fp.person_id
		WHERE f.sha256 = ? AND f.model = ? ORDER BY f.x`, a.userID, a.SHA256, facesModel)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	out := []FaceRef{}
	for rows.Next() {
		var f FaceRef
		if rows.Scan(&f.ID, &f.Box[0], &f.Box[1], &f.Box[2], &f.Box[3], &f.Person, &f.Name) == nil {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"faces": out})
}

// handleFaceThumb: GET /api/v1/faces/{id}/thumb → the face cut out of its
// photo's preview, square with some room around it, 200 px.
func (s *Server) handleFaceThumb(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var sha string
	var x, y, fw, fh float64
	err = s.db.QueryRowContext(r.Context(), `SELECT f.sha256, f.x, f.y, f.w, f.h FROM faces f
		WHERE f.id = ? AND EXISTS (SELECT 1 FROM assets a WHERE a.sha256 = f.sha256 AND a.user_id = ? AND a.deleted_at IS NULL)`,
		id, sessionFrom(r).UserID).Scan(&sha, &x, &y, &fw, &fh)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cache := filepath.Join(s.thumbs, "faces", strconv.FormatInt(id, 10)+".jpg")
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=604800")
	if _, err := os.Stat(cache); err == nil {
		http.ServeFile(w, r, cache)
		return
	}
	a, err := s.scanAsset(s.db.QueryRowContext(r.Context(), `SELECT `+assetCols+` FROM assets WHERE sha256 = ? AND deleted_at IS NULL LIMIT 1`, sha))
	if err != nil || s.ensureThumbs(r.Context(), a) != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.thumbPath(sha, "preview"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	src, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b := src.Bounds()
	W, H := float64(b.Dx()), float64(b.Dy())
	side := math.Max(fw*W, fh*H) * 1.6
	cx, cy := (x+fw/2)*W, (y+fh/2)*H
	rect := image.Rect(int(cx-side/2), int(cy-side/2), int(cx+side/2), int(cy+side/2)).Intersect(b)
	out := image.NewRGBA(image.Rect(0, 0, 200, 200))
	draw.CatmullRom.Scale(out, out.Bounds(), src, rect, draw.Src, nil)
	os.MkdirAll(filepath.Dir(cache), 0o755)
	if tmp, err := os.CreateTemp(filepath.Dir(cache), ".face-"); err == nil {
		jpeg.Encode(tmp, out, &jpeg.Options{Quality: 85})
		tmp.Close()
		os.Rename(tmp.Name(), cache)
	}
	jpeg.Encode(w, out, &jpeg.Options{Quality: 85})
}
