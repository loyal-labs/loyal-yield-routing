package admin

import (
	"net/http"
	"strings"
)

const CSRFHeader = "X-Loyal-Admin"

func sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == "https://"+r.Host
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/login" && !safeMethod(r.Method) && !sameOrigin(r):
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case strings.HasPrefix(r.URL.Path, "/api/admin/") && !safeMethod(r.Method) &&
			(r.Header.Get(CSRFHeader) != "1" || !sameOrigin(r)):
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
