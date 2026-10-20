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

// Handler serves files from dist; a path with no file and no extension is a
// client route and gets index.html.
//
// Seed body. G5 ports the cache tiers of wen server/internal/webserve
// (webserve.go:24-30,71-80): hashed /assets immutable, index.html no-cache.
func Handler() http.Handler {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		// public/ has folders (agents/, landing/) that share a name with a
		// client route; a folder is never served, the route is.
		if info, err := fs.Stat(dist, name); err != nil || info.IsDir() {
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}
