// Package webui serves the embedded local control panel.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed assets/*
var assets embed.FS

// Handler serves only the three embedded UI resources. It never exposes a
// filesystem directory and applies a restrictive same-origin policy.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := ""
		switch r.URL.Path {
		case "/":
			name = "assets/index.html"
		case "/assets/app.css":
			name = "assets/app.css"
		case "/assets/app.js":
			name = "assets/app.js"
		default:
			http.NotFound(w, r)
			return
		}
		if path.Clean(r.URL.Path) != r.URL.Path || strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			http.Error(w, "asset unavailable", http.StatusInternalServerError)
			return
		}
		switch name {
		case "assets/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case "assets/app.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case "assets/app.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	})
}
