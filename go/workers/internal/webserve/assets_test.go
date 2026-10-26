package webserve

import (
	"errors"
	"io/fs"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestAssetsSurviveReleaseChange(t *testing.T) {
	current := fstest.MapFS{
		"index.html":        {Data: []byte("new page")},
		"assets/current.js": {Data: []byte("new code")},
	}
	retained := fstest.MapFS{
		"previous.js":  {Data: []byte("old code")},
		"previous.css": {Data: []byte("old styles")},
		"current.js":   {Data: []byte("must not shadow current build")},
		"index.html":   {Data: []byte("must not become the page")},
		"private.txt":  {Data: []byte("must not escape assets")},
	}
	site := New(WithAssetFallback(current, retained))
	for _, tc := range []struct {
		path, body, cache string
		status            int
	}{
		{"/", "new page", noCache, 200},
		{"/assets/current.js", "new code", immutableCacheControl, 200},
		{"/assets/previous.js", "old code", immutableCacheControl, 200},
		{"/assets/previous.css", "old styles", immutableCacheControl, 200},
		{"/assets/missing.js", "404 page not found\n", noStore, 404},
		{"/private.txt", "404 page not found\n", noStore, 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			site.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.status || w.Body.String() != tc.body || w.Header().Get("Cache-Control") != tc.cache {
				t.Fatalf("got %d %q cache=%q", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
			}
		})
	}
	if _, err := WithAssetFallback(current, retained).Open("assets/../private.txt"); err == nil {
		t.Fatal("invalid asset path escaped into the archive")
	}
	if _, err := fs.ReadFile(WithAssetFallback(current, fstest.MapFS{}), "assets/missing.js"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing archive should preserve missing-file error: %v", err)
	}
}
