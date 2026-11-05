package webserve

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strconv"
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

func (s Site) NotFound(w http.ResponseWriter, r *http.Request) {
	body, err := fs.ReadFile(s.dist, "404/index.html")
	if err != nil {
		s.Shell(w, r, http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", noCache)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (s Site) Page(w http.ResponseWriter, r *http.Request, status int) bool {
	pagePath := path.Clean(r.URL.Path)
	body, err := fs.ReadFile(s.dist, s.pageFile(pagePath, r.URL.Query()))
	if err != nil {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Cache-Control", noStore)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return true
	}
	w.Header().Set("Cache-Control", noCache)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	return true
}

func (s Site) pageFile(pagePath string, query url.Values) string {
	if pagePath == "/" {
		return "__root/index.html"
	}
	if pagePath == "/blog" {
		last := 1
		for s.exists(blogPageFile(last + 1)) {
			last++
		}
		if page := min(parsePage(query.Get("page")), last); page > 1 {
			return blogPageFile(page)
		}
	}
	return strings.TrimPrefix(pagePath, "/") + "/index.html"
}

func blogPageFile(page int) string {
	return fmt.Sprintf("blog/__page/%d/index.html", page)
}

func (s Site) exists(name string) bool {
	_, err := fs.Stat(s.dist, name)
	return err == nil
}

func parsePage(value string) int {
	value = strings.TrimPrefix(strings.TrimLeft(value, " \t\n\v\f\r"), "+")
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	page, _ := strconv.Atoi(value[:end])
	return max(page, 1)
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
