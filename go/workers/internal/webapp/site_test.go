package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestErrorPagesPreserveShellAndHTTPStatus(t *testing.T) {
	const shell = `<!doctype html><div id="root"></div><script src="/assets/app.js"></script>`
	files := fstest.MapFS{
		"index.html": {Data: []byte(shell)}, "assets/app.js": {Data: []byte("/* app */")},
		"page-paths.json":                        {Data: []byte(`["/", "/app", "/app/settings", "/ru/earn", "/blog", "/blog/published.post", "/dev/txv1", "/500"]`)},
		".well-known/apple-app-site-association": {Data: []byte(`{"applinks":{}}`)},
	}
	for _, tc := range []struct {
		path, environment string
		status            int
	}{
		{"/500", "prod", 500}, {"/500/?retry=1", "dev", 500},
		{"/dev/txv1", "prod", 404}, {"/dev/txv1/?probe=1", "prod", 404},
		{"/dev/txv1", "dev", 200}, {"/dev/txv1", "local", 200},
		{"/app", "prod", 200},
		{"/", "prod", 200}, {"/app/settings/?tab=wallet", "prod", 200},
		{"/ru/earn", "prod", 200}, {"/blog", "prod", 200},
		{"/blog/published.post/", "prod", 200},
		{"/missing-page", "prod", 404}, {"/missing-page/?retry=1", "prod", 404},
		{"/app/missing", "prod", 404}, {"/blog/missing-post", "prod", 404},
		{"/assets", "prod", 404}, {"/missing.html", "prod", 404},
		{"/assets/missing.js", "prod", 404},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.path+"/"+tc.environment+"/"+method, func(t *testing.T) {
				w := httptest.NewRecorder()
				site(Config{Dist: files, Environment: tc.environment}).ServeHTTP(w, httptest.NewRequest(method, tc.path, nil))
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d", w.Code, tc.status)
				}
				if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
					t.Fatal("SPA shell must remain HTML")
				}
				if method == http.MethodGet && w.Body.String() != shell {
					t.Fatal("SPA shell was replaced")
				}
				if method == http.MethodHead && w.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
				if tc.status >= 400 && w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("error response must not be cached")
				}
			})
		}
	}
	w := httptest.NewRecorder()
	site(Config{Dist: files, Environment: "prod"}).ServeHTTP(w, httptest.NewRequest("GET", "/assets/app.js", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatal("asset cache behavior changed")
	}
	w = httptest.NewRecorder()
	site(Config{Dist: files}).ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/apple-app-site-association", nil))
	if w.Code != 200 || w.Body.String() != `{"applinks":{}}` {
		t.Fatal("extensionless public file was replaced")
	}
	w = httptest.NewRecorder()
	site(Config{Dist: fstest.MapFS{}, Environment: "prod"}).ServeHTTP(w, httptest.NewRequest("GET", "/500", nil))
	if w.Code != 503 {
		t.Fatalf("missing build returned %d instead of 503", w.Code)
	}
}

func TestPageManifestRequiredForReadiness(t *testing.T) {
	for _, manifest := range []string{"", "not json", "null", "[]", `["/"]`} {
		files := fstest.MapFS{"index.html": {Data: []byte("shell")}}
		if manifest != "" {
			files["page-paths.json"] = &fstest.MapFile{Data: []byte(manifest)}
		}
		cfg := Config{Dist: files}
		want := http.StatusServiceUnavailable
		if manifest == `["/"]` {
			want = http.StatusOK
		}
		w := httptest.NewRecorder()
		ready(cfg.ready()).ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		if w.Code != want {
			t.Fatalf("manifest %q: ready status %d, want %d", manifest, w.Code, want)
		}
		w = httptest.NewRecorder()
		site(cfg).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != want {
			t.Fatalf("manifest %q: page status %d, want %d", manifest, w.Code, want)
		}
	}
}
