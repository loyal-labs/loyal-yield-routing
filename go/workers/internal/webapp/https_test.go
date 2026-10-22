package webapp

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPSRedirectPrecedesHandlersAndPreservesTarget(t *testing.T) {
	for _, host := range []string{"app.askloyal.com", "next.askloyal.com"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			t.Run(host+"/"+method, func(t *testing.T) {
				called := false
				handler := redirectHTTPS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(204) }))
				request := httptest.NewRequest(method, "http://"+host+"/app/a%2Fb?tab=earn%20max&next=%2Fapp", nil)
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set("X-Forwarded-Proto", "http")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if called || response.Code != http.StatusPermanentRedirect {
					t.Fatalf("handler called=%v, status=%d", called, response.Code)
				}
				want := "https://" + host + "/app/a%2Fb?tab=earn%20max&next=%2Fapp"
				if got := response.Header().Get("Location"); got != want {
					t.Fatalf("Location=%q, want %q", got, want)
				}
			})
		}
	}
}

func TestHTTPSRedirectAllowsSecureAndLocalRequests(t *testing.T) {
	for _, tc := range []struct {
		name, peer, proto string
		tls               bool
		status            int
	}{
		{"tunnel HTTPS", "127.0.0.1:12345", "https", false, 200},
		{"local readiness", "127.0.0.1:12345", "", false, 200},
		{"untrusted forwarding header", "192.0.2.1:12345", "http", false, 200},
		{"direct TLS", "127.0.0.1:12345", "http", true, 200},
		{"IPv6 tunnel HTTP", "[::1]:12345", "http", false, 308},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(Config{Ready: func() bool { return true }})
			request := httptest.NewRequest("GET", "http://next.askloyal.com/readyz", nil)
			request.RemoteAddr = tc.peer
			request.Header.Set("X-Forwarded-Proto", tc.proto)
			if tc.tls {
				request.TLS = &tls.ConnectionState{}
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d, want %d", response.Code, tc.status)
			}
		})
	}
}
