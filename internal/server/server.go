// Package server is trunk's HTTP API.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/carnager/trunk/internal/db"
	"github.com/carnager/trunk/internal/media"
)

// Config holds the server settings.
type Config struct {
	DataDir    string
	TrashDays  int   // days before trashed assets are deleted for good
	MaxUpload  int64 // bytes per file
	TrustProxy bool  // honour X-Forwarded-For from a reverse proxy
	Version    string
}

// Server owns the data directory and serves the API.
type Server struct {
	cfg       Config
	db        *sql.DB
	log       *slog.Logger
	originals string
	thumbs    string
	uploads   string

	limit  limiter
	seenMu sync.Mutex
	seen   map[int64]time.Time

	ingestMu sync.Mutex // serialises duplicate check, file placement and insert

	thumbQ    chan int64
	thumbSem  chan struct{}
	thumbMu   sync.Mutex
	thumbLock map[string]*refMutex
	thumbFail map[string]time.Time

	uploadMu   sync.Mutex
	uploadBusy map[string]bool
}

// New opens the data directory, creating it if needed.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	if cfg.TrashDays <= 0 {
		cfg.TrashDays = 30
	}
	if cfg.MaxUpload <= 0 {
		cfg.MaxUpload = 50 << 30
	}
	s := &Server{
		cfg:       cfg,
		log:       log,
		originals: filepath.Join(cfg.DataDir, "originals"),
		thumbs:    filepath.Join(cfg.DataDir, "thumbs"),
		uploads:   filepath.Join(cfg.DataDir, "uploads"),
		limit:     limiter{fail: map[string][]time.Time{}},
		seen:      map[int64]time.Time{},
		thumbQ:    make(chan int64, 4096),
		// Each render holds a decoded original (about 50 MB for 12 MP), so not one per core.
		thumbSem:   make(chan struct{}, min(8, max(1, runtime.NumCPU()/2))),
		thumbLock:  map[string]*refMutex{},
		thumbFail:  map[string]time.Time{},
		uploadBusy: map[string]bool{},
	}
	for _, d := range []string{s.originals, s.thumbs, s.uploads} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	d, err := db.Open(filepath.Join(cfg.DataDir, "trunk.db"))
	if err != nil {
		return nil, err
	}
	s.db = d
	return s, nil
}

// DB exposes the database for the command line tools.
func (s *Server) DB() *sql.DB { return s.db }

// Close releases the database.
func (s *Server) Close() error { return s.db.Close() }

// Run starts the background workers; they stop when ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	for range cap(s.thumbSem) {
		go s.thumbWorker(ctx)
	}
	go s.janitor(ctx)
}

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1/server", s.handleServerInfo)
	m.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	m.HandleFunc("POST /api/v1/auth/logout", s.auth(s.handleLogout))
	m.HandleFunc("GET /api/v1/me", s.auth(s.handleMe))
	m.HandleFunc("GET /api/v1/devices", s.auth(s.handleDevices))
	m.HandleFunc("DELETE /api/v1/devices/{id}", s.auth(s.handleRevokeDevice))

	m.HandleFunc("GET /api/v1/changes", s.auth(s.handleChanges))
	m.HandleFunc("POST /api/v1/assets/check", s.auth(s.handleCheck))
	m.HandleFunc("POST /api/v1/assets", s.auth(s.handleSimpleUpload))
	m.HandleFunc("GET /api/v1/assets/{id}", s.auth(s.handleGetAsset))
	m.HandleFunc("PATCH /api/v1/assets/{id}", s.auth(s.handlePatchAsset))
	m.HandleFunc("GET /api/v1/assets/{id}/file", s.auth(s.handleFile))
	m.HandleFunc("GET /api/v1/assets/{id}/original", s.auth(s.handleOriginal))
	m.HandleFunc("PUT /api/v1/assets/{id}/edit", s.auth(s.handlePutEdit))
	m.HandleFunc("DELETE /api/v1/assets/{id}/edit", s.auth(s.handleDeleteEdit))
	m.HandleFunc("GET /api/v1/assets/{id}/thumb/{size}", s.auth(s.handleThumb))
	m.HandleFunc("POST /api/v1/assets/trash", s.auth(s.handleTrash))
	m.HandleFunc("POST /api/v1/assets/restore", s.auth(s.handleRestore))
	m.HandleFunc("POST /api/v1/assets/delete", s.auth(s.handleDelete))

	m.HandleFunc("POST /api/v1/albums", s.auth(s.handleCreateAlbum))
	m.HandleFunc("GET /api/v1/albums/{id}", s.auth(s.handleGetAlbum))
	m.HandleFunc("PATCH /api/v1/albums/{id}", s.auth(s.handlePatchAlbum))
	m.HandleFunc("DELETE /api/v1/albums/{id}", s.auth(s.handleDeleteAlbum))
	m.HandleFunc("POST /api/v1/albums/{id}/add", s.auth(s.handleAlbumAdd))
	m.HandleFunc("POST /api/v1/albums/{id}/remove", s.auth(s.handleAlbumRemove))

	m.HandleFunc("OPTIONS /api/v1/uploads", s.handleTusOptions)
	m.HandleFunc("OPTIONS /api/v1/uploads/{id}", s.handleTusOptions)
	m.HandleFunc("POST /api/v1/uploads", s.auth(s.handleTusCreate))
	m.HandleFunc("HEAD /api/v1/uploads/{id}", s.auth(s.handleTusHead))
	m.HandleFunc("PATCH /api/v1/uploads/{id}", s.auth(s.handleTusPatch))
	m.HandleFunc("DELETE /api/v1/uploads/{id}", s.auth(s.handleTusDelete))

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpError(w, http.StatusNotFound, "not found")
	})
	return s.logRequests(m)
}

func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       "trunk",
		"version":    s.cfg.Version,
		"api":        1,
		"trash_days": s.cfg.TrashDays,
		"max_upload": s.cfg.MaxUpload,
		"features": map[string]bool{
			"video_posters": media.HaveFFmpeg(),
			"tus":           true,
			"edits":         true,
			"source_sha256": true,
			"albums":        true,
		},
		"thumb_sizes": media.Sizes,
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int)           { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		if sw.status >= 400 || r.Method != http.MethodGet {
			s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"dur", time.Since(start).Round(time.Millisecond))
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error("internal error", "err", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpError(w, http.StatusBadRequest, "bad id")
		return 0, false
	}
	return id, true
}
