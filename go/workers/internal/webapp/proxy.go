package webapp

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// forward sends a request to the Next deployment that still owns the path
// (every /api and /ingest path Go does not answer; /ingest is not a rewrite —
// apps/web has no rewrites, it is a route handler of Next's,
// web/apps/web/src/app/ingest/[...path]/route.ts).
//
// The contract:
//
//   - method, body, query and cookies pass through;
//   - the outbound Host is the upstream's host (Vercel routes by it);
//   - X-Forwarded-For/Host/Proto describe the inbound request;
//   - wallet challenge/completion Origins stay public so SIWS names the app;
//   - other Origins whose host equals the inbound Host are presented as the
//     upstream origin: /api/observability/events rejects an Origin that is
//     not its own URL origin, which through the proxy means the upstream's;
//   - Set-Cookie comes back untouched, Domain=askloyal.com and all;
//   - SSE streams flush immediately;
//   - upstream failures are a logged 502, never a panic.
func forward(upstream *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.SetXForwarded()
			presentOrigin(r, upstream)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("loyal-web proxy", "error", err, "method", r.Method, "path", r.URL.Path)
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		},
	}
	return proxy
}

func presentOrigin(r *httputil.ProxyRequest, upstream *url.URL) {
	// Wallets bind SIWS to the browser origin, not the private API upstream.
	if r.In.URL.Path == "/api/auth/wallet/challenge" || r.In.URL.Path == "/api/auth/wallet/complete" {
		return
	}
	origin := r.Out.Header.Get("Origin")
	if origin == "" {
		return
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.Host != r.In.Host {
		return
	}
	r.Out.Header.Set("Origin", upstream.Scheme+"://"+upstream.Host)
}
