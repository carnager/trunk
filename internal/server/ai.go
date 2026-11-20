package server

import (
	"cmp"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
)

// Content search. trunk runs no model itself: the machines it runs on are
// often too small for that. An indexer elsewhere (trunk-ai, on a desktop)
// fetches thumbnails, computes a vector per photo with an image model and
// sends it here; clients turn a search text into a vector with the matching
// text model (which trunk keeps for them to download) and send that. trunk
// stores the vectors and compares, which is plain arithmetic.

// Vectors are unit length, so their dot product is their cosine similarity.

var reModelName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// The files a client needs to make search vectors itself.
var modelFiles = map[string]bool{"text.onnx": true, "vocab.txt": true, "dense.safetensors": true, "model.json": true}

type vecCache struct {
	mu   sync.Mutex
	byID map[string]map[string][]float32 // model → sha256 → vector
}

func (c *vecCache) model(s *Server, model string) (map[string][]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string]map[string][]float32{}
	}
	if m, ok := c.byID[model]; ok {
		return m, nil
	}
	rows, err := s.db.Query(`SELECT sha256, vec FROM embeddings WHERE model = ?`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string][]float32{}
	for rows.Next() {
		var sha string
		var raw []byte
		if err := rows.Scan(&sha, &raw); err != nil {
			return nil, err
		}
		m[sha] = decodeVec(raw)
	}
	c.byID[model] = m
	return m, rows.Err()
}

func (c *vecCache) put(model, sha string, v []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.byID[model]; ok {
		m[sha] = v
	}
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

func adminOnly(w http.ResponseWriter, r *http.Request) bool {
	if !sessionFrom(r).Admin {
		httpError(w, http.StatusForbidden, "only for an admin account")
		return false
	}
	return true
}

func modelParam(w http.ResponseWriter, m string) (string, bool) {
	if !reModelName.MatchString(m) {
		httpError(w, http.StatusBadRequest, "model: a short lower-case name")
		return "", false
	}
	return m, true
}

// handleAITodo: GET /api/v1/ai/todo?model=M[&limit=N] (admin) → photos and
// videos of the whole household without a vector for that model yet, one per
// file content: {"assets": [{"id", "sha256"}], "remaining": n}.
func (s *Server) handleAITodo(w http.ResponseWriter, r *http.Request) {
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
	const missing = `FROM assets a WHERE a.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.sha256 = a.sha256 AND e.model = ?)`
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
		if err := rows.Scan(&t.ID, &t.SHA256); err != nil {
			s.internal(w, err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": out, "remaining": remaining})
}

// handleAIThumb: GET /api/v1/ai/thumb/{id} (admin) → any asset's medium
// thumbnail, for the indexer, which works for everyone in the household.
func (s *Server) handleAIThumb(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, err := s.scanAsset(s.db.QueryRowContext(r.Context(), `SELECT `+assetCols+` FROM assets WHERE id = ? AND deleted_at IS NULL`, id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.ensureThumbs(r.Context(), a); err != nil {
		httpError(w, http.StatusNotFound, "no thumbnail for this asset")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, s.thumbPath(a.SHA256, "medium"))
}

// handleAIEmbeddings: PUT /api/v1/ai/embeddings (admin)
// {"model", "items": [{"sha256", "vec": base64 of little-endian float32s}]}.
func (s *Server) handleAIEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	var req struct {
		Model string `json:"model"`
		Items []struct {
			SHA256 string `json:"sha256"`
			Vec    string `json:"vec"`
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
	type entry struct {
		sha string
		raw []byte
	}
	var entries []entry
	for _, it := range req.Items {
		raw, err := base64.StdEncoding.DecodeString(it.Vec)
		if err != nil || len(raw) == 0 || len(raw)%4 != 0 || len(raw) > 4*4096 || len(it.SHA256) != 64 {
			httpError(w, http.StatusBadRequest, "bad item "+it.SHA256)
			return
		}
		entries = append(entries, entry{it.SHA256, raw})
	}
	err := s.inTx(r.Context(), func(tx *sql.Tx, _, _ int64) error {
		for _, e := range entries {
			if _, err := tx.ExecContext(r.Context(), `INSERT OR REPLACE INTO embeddings (sha256, model, vec) VALUES (?, ?, ?)`, e.sha, model, e.raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, e := range entries {
		s.vecs.put(model, e.sha, decodeVec(e.raw))
	}
	writeJSON(w, http.StatusOK, map[string]int{"stored": len(entries)})
}

// Search results: the best match and everything not much worse, so a search
// for something the library does not have brings a few near misses rather
// than a page of noise. Tuned on clip-ViT-B-32 (multilingual), whose scores
// for real matches lie around 0.26–0.31.
const (
	searchMinScore = 0.255
	searchWindow   = 0.035
)

// handleAISearch: POST /api/v1/ai/search {"model", "vec": base64, "limit"}
// → {"results": [{"id", "score"}]}: the caller's own photos and videos, best
// first.
func (s *Server) handleAISearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Vec   string `json:"vec"`
		Limit int    `json:"limit"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	model, ok := modelParam(w, req.Model)
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Vec)
	if err != nil || len(raw) == 0 || len(raw)%4 != 0 {
		httpError(w, http.StatusBadRequest, "vec: base64 of little-endian float32s")
		return
	}
	q := decodeVec(raw)
	if req.Limit <= 0 || req.Limit > 1000 {
		req.Limit = 300
	}
	vecs, err := s.vecs.model(s, model)
	if err != nil {
		s.internal(w, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, sha256 FROM assets
		WHERE user_id = ? AND deleted_at IS NULL AND trashed_at IS NULL`, sessionFrom(r).UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	type hit struct {
		ID    int64   `json:"id"`
		Score float32 `json:"score"`
	}
	var hits []hit
	for rows.Next() {
		var id int64
		var sha string
		if rows.Scan(&id, &sha) != nil {
			continue
		}
		v := vecs[sha]
		if len(v) != len(q) {
			continue
		}
		var dot float32
		for i := range q {
			dot += q[i] * v[i]
		}
		hits = append(hits, hit{id, dot})
	}
	rows.Close()
	slices.SortFunc(hits, func(a, b hit) int { return cmp.Compare(b.Score, a.Score) })
	out := []hit{}
	if len(hits) > 0 {
		floor := max(searchMinScore, hits[0].Score-searchWindow)
		for _, h := range hits {
			if h.Score < floor || len(out) >= req.Limit {
				break
			}
			out = append(out, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// --- the text model, for clients ---

func (s *Server) modelDir(model string) string { return filepath.Join(s.cfg.DataDir, "ai", model) }

// currentModel is the model whose files clients can download, or "".
func (s *Server) currentModel() string {
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "ai", "current"))
	if err != nil || !reModelName.Match(b) {
		return ""
	}
	return string(b)
}

// handleAIModelPut: PUT /api/v1/ai/model/{model}/{file} (admin): a file of
// the text model; once all are there, the model becomes the current one.
func (s *Server) handleAIModelPut(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	model, ok := modelParam(w, r.PathValue("model"))
	if !ok {
		return
	}
	name := r.PathValue("file")
	if !modelFiles[name] {
		httpError(w, http.StatusBadRequest, "unknown model file")
		return
	}
	dir := s.modelDir(model)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		s.internal(w, err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		s.internal(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, 2<<30)); err != nil {
		tmp.Close()
		httpError(w, http.StatusBadRequest, "upload failed")
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		s.internal(w, err)
		return
	}
	complete := true
	for f := range modelFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			complete = false
		}
	}
	if complete {
		os.WriteFile(filepath.Join(s.cfg.DataDir, "ai", "current"), []byte(model), 0o640)
	}
	w.WriteHeader(http.StatusNoContent)
}

type modelFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var fileHashes sync.Map // path+mtime → sha256

// handleAIModel: GET /api/v1/ai/model → {"model", "files": [...]}, the text
// model clients download to make search vectors; 404 while there is none.
func (s *Server) handleAIModel(w http.ResponseWriter, r *http.Request) {
	model := s.currentModel()
	if model == "" {
		httpError(w, http.StatusNotFound, "no model for content search yet; run trunk-ai")
		return
	}
	var files []modelFile
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for f := range modelFiles {
			if !yield(f) {
				return
			}
		}
	}) {
		p := filepath.Join(s.modelDir(model), name)
		st, err := os.Stat(p)
		if err != nil {
			httpError(w, http.StatusNotFound, "model incomplete")
			return
		}
		key := p + st.ModTime().String()
		sum, ok := fileHashes.Load(key)
		if !ok {
			h, err := hashFile(p)
			if err != nil {
				s.internal(w, err)
				return
			}
			sum = h
			fileHashes.Store(key, h)
		}
		files = append(files, modelFile{name, st.Size(), sum.(string)})
	}
	var n int
	s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM embeddings WHERE model = ?`, model).Scan(&n)
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "files": files, "indexed": n})
}

// handleAIModelFile: GET /api/v1/ai/model/{model}/{file}.
func (s *Server) handleAIModelFile(w http.ResponseWriter, r *http.Request) {
	model, ok := modelParam(w, r.PathValue("model"))
	name := r.PathValue("file")
	if !ok || !modelFiles[name] {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.modelDir(model), name)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, p)
}
