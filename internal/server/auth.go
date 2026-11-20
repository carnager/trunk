package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (RFC 9106 second recommendation, lighter memory).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// HashPassword returns a PHC-formatted argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// CheckPassword verifies pw against a hash from HashPassword.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash makes logins for unknown users cost as much as real ones.
var dummyHash, _ = HashPassword("trunk-timing-equaliser")

func newToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = "trk_" + base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// Session is the authenticated caller of a request.
type Session struct {
	UserID   int64
	UserName string
	Admin    bool
	DeviceID int64
}

type ctxKey struct{}

func sessionFrom(r *http.Request) *Session {
	s, _ := r.Context().Value(ctxKey{}).(*Session)
	return s
}

// auth wraps h so it only runs with a valid device token.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="trunk"`)
			httpError(w, http.StatusUnauthorized, "missing token")
			return
		}
		var sess Session
		var admin int
		err := s.db.QueryRowContext(r.Context(), `
			SELECT d.id, u.id, u.name, u.is_admin FROM devices d JOIN users u ON u.id = d.user_id
			WHERE d.token_hash = ?`, hashToken(tok)).Scan(&sess.DeviceID, &sess.UserID, &sess.UserName, &admin)
		if errors.Is(err, sql.ErrNoRows) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="trunk", error="invalid_token"`)
			httpError(w, http.StatusUnauthorized, "invalid token")
			return
		} else if err != nil {
			s.internal(w, err)
			return
		}
		sess.Admin = admin == 1
		s.touchDevice(sess.DeviceID)
		ctx := context.WithValue(r.Context(), ctxKey{}, &sess)
		h(w, r.WithContext(withLanguage(ctx, r)))
	}
}

// touchDevice records device activity, at most once a minute per device.
func (s *Server) touchDevice(id int64) {
	now := time.Now()
	s.seenMu.Lock()
	last := s.seen[id]
	if now.Sub(last) < time.Minute {
		s.seenMu.Unlock()
		return
	}
	s.seen[id] = now
	s.seenMu.Unlock()
	s.db.Exec(`UPDATE devices SET last_seen_at = ? WHERE id = ?`, now.UnixMilli(), id)
}

// limiter slows password guessing: after 5 failures from one client within
// 15 minutes, further attempts are refused until the window passes.
type limiter struct {
	mu   sync.Mutex
	fail map[string][]time.Time
}

const (
	limitWindow = 15 * time.Minute
	limitMax    = 5
)

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= limitMax
}

func (l *limiter) failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail[key] = append(l.prune(key), time.Now())
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fail, key)
}

func (l *limiter) prune(key string) []time.Time {
	ts := l.fail[key]
	cut := time.Now().Add(-limitWindow)
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fail, key)
	} else {
		l.fail[key] = ts
	}
	return ts
}

// clientIP returns the caller's address. Behind a reverse proxy (TrustProxy),
// the last X-Forwarded-For entry is the one the proxy itself added.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Device   string `json:"device"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ip := s.clientIP(r)
	if s.limit.blocked(ip) {
		w.Header().Set("Retry-After", "900")
		httpError(w, http.StatusTooManyRequests, "too many failed logins, try again later")
		return
	}
	var uid int64
	var hash string
	err := s.db.QueryRowContext(r.Context(), `SELECT id, pw_hash FROM users WHERE name = ?`, req.User).Scan(&uid, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		CheckPassword(dummyHash, req.Password)
		s.limit.failed(ip)
		httpError(w, http.StatusUnauthorized, "wrong user or password")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	if !CheckPassword(hash, req.Password) {
		s.limit.failed(ip)
		s.log.Warn("failed login", "user", req.User, "ip", ip)
		httpError(w, http.StatusUnauthorized, "wrong user or password")
		return
	}
	s.limit.reset(ip)

	name := strings.TrimSpace(req.Device)
	if name == "" {
		name = "unnamed device"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	tok, th, err := newToken()
	if err != nil {
		s.internal(w, err)
		return
	}
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(r.Context(), `
		INSERT INTO devices (user_id, name, token_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		uid, name, th, now, now)
	if err != nil {
		s.internal(w, err)
		return
	}
	id, _ := res.LastInsertId()
	s.log.Info("device logged in", "user", req.User, "device", name, "ip", ip)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "device_id": id})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM devices WHERE id = ?`, sess.DeviceID); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": sess.UserID, "name": sess.UserName, "admin": sess.Admin, "device_id": sess.DeviceID,
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, name, created_at, last_seen_at FROM devices WHERE user_id = ? ORDER BY last_seen_at DESC`, sess.UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	type device struct {
		ID       int64     `json:"id"`
		Name     string    `json:"name"`
		Created  time.Time `json:"created_at"`
		LastSeen time.Time `json:"last_seen_at"`
		Current  bool      `json:"current"`
	}
	list := []device{}
	for rows.Next() {
		var d device
		var c, l int64
		if err := rows.Scan(&d.ID, &d.Name, &c, &l); err != nil {
			s.internal(w, err)
			return
		}
		d.Created, d.LastSeen = time.UnixMilli(c).UTC(), time.UnixMilli(l).UTC()
		d.Current = d.ID == sess.DeviceID
		list = append(list, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": list})
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM devices WHERE id = ? AND user_id = ?`, id, sess.UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "no such device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
