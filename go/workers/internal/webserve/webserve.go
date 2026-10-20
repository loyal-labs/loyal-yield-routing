// Package webserve serves the built web app (web/apps/loyal/dist), embedded at
// build time by `bun run web:build`.
package webserve

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

var (
	dist    fs.FS
	distErr error
)

func init() {
	dist, distErr = fs.Sub(embedded, "dist")
}

// Cache tiers, ported from wen server/internal/webserve (webserve.go:24-30,
// 71-80): hashed /assets are immutable for a year, index.html and every SPA
// fallback are revalidated, other public files cache for a day.
const (
	immutableCacheControl  = "public, max-age=31536000, immutable"
	publicFileCacheControl = "public, max-age=86400, stale-while-revalidate=604800"
	noCache                = "no-cache"
	noStore                = "no-store"
)

// Ready reports whether the embedded build contains index.html: a binary
// built without the web build must not report ready.
func Ready() bool {
	if distErr != nil {
		return false
	}
	_, err := fs.Stat(dist, "index.html")
	return err == nil
}

// Handler serves files from dist; a path with no file and no extension is a
// client route and gets index.html.
func Handler() http.Handler {
	if distErr != nil {
		panic(distErr)
	}
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(dist, name); err != nil {
			if path.Ext(name) != "" {
				w.Header().Set("Cache-Control", noStore)
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			name = "index.html"
		}
		switch {
		case name == "index.html":
			w.Header().Set("Cache-Control", noCache)
		case strings.HasPrefix(name, "assets/"):
			w.Header().Set("Cache-Control", immutableCacheControl)
		case publicFile(name):
			w.Header().Set("Cache-Control", publicFileCacheControl)
		default:
			w.Header().Set("Cache-Control", noCache)
		}
		files.ServeHTTP(w, r)
	})
}

// publicFile matches the file kinds the browser reuses across pages
// (webserve.go:86-92 of the wen port).
func publicFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico", ".avif", ".woff", ".woff2":
		return true
	}
	return false
}
