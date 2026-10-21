package webserve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestErrorPagesPreserveShellAndHTTPStatus(t *testing.T) {
	const shell = `<!doctype html><div id="root"></div><script src="/assets/app.js"></script>`
	files := fstest.MapFS{"index.html": {Data: []byte(shell)}, "assets/app.js": {Data: []byte("/* app */")}}
	for _, tc := range []struct {
		path, environment string
		status            int
	}{
		{"/500", "prod", 500}, {"/500/?retry=1", "dev", 500},
		{"/dev/txv1", "prod", 404}, {"/dev/txv1/?probe=1", "prod", 404},
		{"/dev/txv1", "dev", 200}, {"/dev/txv1", "local", 200},
		{"/app", "prod", 200},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.path+"/"+tc.environment+"/"+method, func(t *testing.T) {
				w := httptest.NewRecorder()
				handler(files, tc.environment).ServeHTTP(w, httptest.NewRequest(method, tc.path, nil))
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
				if tc.status >= 400 && w.Header().Get("Cache-Control") != noStore {
					t.Fatal("error response must not be cached")
				}
			})
		}
	}
	w := httptest.NewRecorder()
	handler(files, "prod").ServeHTTP(w, httptest.NewRequest("GET", "/assets/app.js", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != immutableCacheControl {
		t.Fatal("asset cache behavior changed")
	}
	w = httptest.NewRecorder()
	handler(fstest.MapFS{}, "prod").ServeHTTP(w, httptest.NewRequest("GET", "/500", nil))
	if w.Code != 503 {
		t.Fatalf("missing build returned %d instead of 503", w.Code)
	}
}
