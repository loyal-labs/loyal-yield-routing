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

const (
	immutableCacheControl  = "public, max-age=31536000, immutable"
	publicFileCacheControl = "public, max-age=86400, stale-while-revalidate=604800"
	noCache                = "no-cache"
	noStore                = "no-store"
)

func Ready() bool {
	if distErr != nil {
		return false
	}
	_, err := fs.Stat(dist, "index.html")
	return err == nil
}

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
		if info, err := fs.Stat(dist, name); err != nil || info.IsDir() {
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

func publicFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico", ".avif", ".woff", ".woff2":
		return true
	}
	return false
}
