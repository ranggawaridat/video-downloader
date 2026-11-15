package httpapi

import (
	"net/http"
	"path/filepath"
)

// staticHandler menyajikan frontend web sederhana dari StaticDir.
func (h *Handler) staticHandler() http.Handler {
	if h.cfg.StaticDir == "" {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.Dir(h.cfg.StaticDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, filepath.Join(h.cfg.StaticDir, "index.html"))
			return
		}
		if r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
			http.ServeFile(w, r, filepath.Join(h.cfg.StaticDir, "admin.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
}
