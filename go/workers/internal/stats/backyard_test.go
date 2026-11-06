package stats

import (
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

// backyard.askloyal.com is public and partner-facing: it must serve only its
// page, its API and static files, never the stats page or other stats APIs.
func TestBackyardHostServesOnlyItsPage(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := backyardHostOnly(ok)
	for _, tc := range []struct {
		host, path string
		status     int
	}{
		{"backyard.askloyal.com", "/", 200},
		{"backyard.askloyal.com", "/api/backyard", 200},
		{"backyard.askloyal.com", "/assets/index-abc.js", 200},
		{"backyard.askloyal.com", "/app-icon.svg", 200},
		{"backyard.askloyal.com", "/api/earn/vaults", 404},
		{"backyard.askloyal.com", "/api/earn/stats", 404},
		{"backyard.askloyal.com", "/api/earn/growth-metrics", 404},
		{"backyard.askloyal.com", "/backyard", 404},
		{"stats.askloyal.com", "/api/earn/vaults", 200},
		{"stats.askloyal.com", "/backyard", 200},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Host = tc.host
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s%s: got %d, want %d", tc.host, tc.path, w.Code, tc.status)
		}
	}
}

// A landed NAV report is read from the NAV adaptor's verified return bytes,
// and otherwise from the built request (the Trustful route has no adaptor).
func TestBackyardReportNAV(t *testing.T) {
	bytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(bytes, 17_079_029)
	program, data := backyardNAVAdaptor, base64.StdEncoding.EncodeToString(bytes)
	if nav, ok := backyardReportNAV(&program, &data, nil); !ok || nav != 17_079_029 {
		t.Fatalf("adaptor return data: got %v %v", nav, ok)
	}
	request := base64.StdEncoding.EncodeToString([]byte(`{"Report":{"NAVAfterRaw":17066770}}`))
	other := "3pnpK9nrs1R65eMV1wqCXkDkhSgN18xb1G5pgYPwoZjJ"
	if nav, ok := backyardReportNAV(&other, &data, &request); !ok || nav != 17_066_770 {
		t.Fatalf("request fallback: got %v %v", nav, ok)
	}
	if _, ok := backyardReportNAV(nil, nil, nil); ok {
		t.Fatal("a report with nothing to read must not yield a NAV")
	}
}
