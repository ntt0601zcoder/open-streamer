// Package mediaserve serves HLS/DASH files from disk under /{code}/… — same URL layout as the API server.
package mediaserve

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Mount registers GET /{code}/index.m3u8, GET /{code}/index.mpd, and GET /{code}/* on r.
func Mount(r chi.Router, hlsDir, dashDir string) {
	s := &roots{hlsDir: hlsDir, dashDir: dashDir}
	r.Get("/{code}/index.m3u8", s.serveManifest("index.m3u8", "application/vnd.apple.mpegurl", s.hlsDir))
	r.Get("/{code}/index.mpd", s.serveManifest("index.mpd", "application/dash+xml", s.dashDir))
	r.Get("/{code}/*", s.serveStreamNested())
}

// NewHandler returns a standalone handler with the same routes as Mount (for tests or embedding).
func NewHandler(hlsDir, dashDir string) http.Handler {
	r := chi.NewRouter()
	Mount(r, hlsDir, dashDir)
	return r
}

type roots struct {
	hlsDir  string
	dashDir string
}

func (s *roots) serveManifest(filename, contentType, rootDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		if code == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		switch filename {
		case "index.mpd":
			w.Header().Set("Cache-Control", "no-store, max-age=0, must-revalidate")
		case "index.m3u8":
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Pragma", "no-cache")
		}
		http.ServeFile(w, r, filepath.Join(rootDir, code, filename))
	}
}

func (s *roots) serveStreamNested() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		suffix := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
		if code == "" || suffix == "" {
			http.NotFound(w, r)
			return
		}
		rel := filepath.Clean(suffix)
		if rel == "." || strings.HasPrefix(rel, "..") {
			http.NotFound(w, r)
			return
		}
		baseName := filepath.Base(rel)
		ext := strings.ToLower(filepath.Ext(baseName))
		baseDir := ""
		switch ext {
		case ".ts", ".m3u8":
			baseDir = s.hlsDir
		case ".m4s", ".mpd", ".mp4":
			baseDir = s.dashDir
		default:
			http.NotFound(w, r)
			return
		}
		switch ext {
		case ".mpd":
			w.Header().Set("Cache-Control", "no-store, max-age=0, must-revalidate")
		case ".m3u8":
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Pragma", "no-cache")
		}
		http.ServeFile(w, r, filepath.Join(baseDir, code, rel))
	}
}
