package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The web client: plain HTML, CSS and JavaScript in web/app, no build step,
// served by trunk itself at /. It talks to the API like the phone app does;
// pictures and videos load with the token in a cookie (reading only, see
// auth).

// webCookie carries the web client's token for <img> and <video>.
const webCookie = "trunk_token"

//go:embed web/app
var webApp embed.FS

func (s *Server) webHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; "+
		"style-src 'self' 'unsafe-inline'; script-src 'self'; font-src 'self'; connect-src 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// handleWebIndex: GET / → the web client.
func (s *Server) handleWebIndex(w http.ResponseWriter, r *http.Request) {
	s.webHeaders(w)
	w.Header().Set("Cache-Control", "no-cache")
	b, err := webApp.ReadFile("web/app/index.html")
	if err != nil {
		s.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// handleWebFile: GET /app/{file} → its scripts, styles and icons.
func (s *Server) handleWebFile(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/app/"))
	sub, _ := fs.Sub(webApp, "web/app")
	if strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	s.webHeaders(w)
	w.Header().Set("Cache-Control", "no-cache") // small files; always the version of this trunk
	http.ServeFileFS(w, r, sub, name)
}
