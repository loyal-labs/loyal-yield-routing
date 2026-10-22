package webserve

import (
	"io/fs"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
)

const (
	immutableCacheControl  = "public, max-age=31536000, immutable"
	publicFileCacheControl = "public, max-age=86400, stale-while-revalidate=604800"
	noCache                = "no-cache"
	noStore                = "no-store"
)

type Site struct {
	dist  fs.FS
	files http.Handler
}

func New(dist fs.FS) Site {
	return Site{dist: dist, files: http.FileServerFS(dist)}
}

func (s Site) Ready() bool {
	_, err := fs.Stat(s.dist, "index.html")
	return err == nil
}

func (s Site) Shell(w http.ResponseWriter, r *http.Request, status int) {
	w.Header().Set("Cache-Control", noStore)
	body, err := fs.ReadFile(s.dist, "index.html")
	if err != nil {
		http.Error(w, "web build missing", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (s Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if info, err := fs.Stat(s.dist, name); err != nil || info.IsDir() {
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
	s.files.ServeHTTP(w, r)
}

func publicFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico", ".avif", ".woff", ".woff2":
		return true
	}
	return false
}

var stamp string

func Release() string {
	if stamp != "" {
		return stamp
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" || modified == "true" {
		return "dirty"
	}
	return revision
}
