package webapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	ingestPrefix        = "/ingest/"
	ingestMaxBody       = 4_500_000
	ingestHeaderTimeout = 30 * time.Second
)

var mixpanelOrigin = &url.URL{Scheme: "https", Host: "api-js.mixpanel.com"}

var ingestRequestHeaders = []string{
	"Accept",
	"Accept-Language",
	"Authorization",
	"CF-Connecting-IP",
	"Content-Type",
	"Forwarded",
	"Origin",
	"Referer",
	"User-Agent",
	"X-Forwarded-For",
	"X-Real-IP",
	"X-Vercel-Forwarded-For",
}

var ingestResponseHeaders = []string{
	"Cache-Control",
	"Content-Encoding",
	"Content-Length",
	"Content-Type",
	"Date",
	"Etag",
	"Vary",
}

// newIngestProxy mirrors web/apps/web/src/app/ingest/[...path]/route.ts. Request bodies are
// buffered because that route buffers them, so the upstream always gets a Content-Length.
func newIngestProxy(origin *url.URL) http.Handler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = ingestHeaderTimeout
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = origin.Scheme
			out.URL.Host = origin.Host
			out.URL.Path = "/" + strings.TrimPrefix(pr.In.URL.Path, ingestPrefix)
			out.URL.RawPath = ""
			out.Host = ""
			out.Header = make(http.Header, len(ingestRequestHeaders))
			for _, name := range ingestRequestHeaders {
				if value := strings.Join(pr.In.Header.Values(name), ", "); value != "" {
					out.Header.Set(name, value)
				}
			}
			if out.Header.Get("Accept") == "" {
				out.Header.Set("Accept", "*/*")
			}
			if out.Header.Get("Accept-Language") == "" {
				out.Header.Set("Accept-Language", "*")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			kept := make(http.Header, len(ingestResponseHeaders))
			for _, name := range ingestResponseHeaders {
				if value := resp.Header.Values(name); len(value) > 0 {
					kept[name] = value
				}
			}
			resp.Header = kept
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				slog.Error("loyal-web ingest proxy", "error", err, "method", r.Method, "path", r.URL.Path)
			}
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions:
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/") {
			target := strings.TrimRight(r.URL.Path, "/")
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", target)
			w.Header().Set("Refresh", "0;url="+target)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusPermanentRedirect)
			io.WriteString(w, "Redirecting...\n")
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodOptions {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ingestMaxBody))
			if err != nil {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		proxy.ServeHTTP(w, r)
	})
}
