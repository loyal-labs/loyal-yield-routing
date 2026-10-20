package webapp

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// forward sends a request to the Next deployment unchanged except for Host.
//
// Seed body. G5 owns the contract: method, body, query and cookies pass
// through; Set-Cookie comes back with no Domain rewrite; SSE streams flush.
func forward(upstream *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.SetXForwarded()
		},
		FlushInterval: -1,
	}
	return proxy
}

// ingest forwards PostHog's /ingest/* to PostHog, as apps/web's rewrite does.
//
// Seed body. G5 maps /ingest/static/* to the assets host as next.config.ts does.
func ingest(posthog *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(posthog)
			r.Out.URL.Path = r.In.URL.Path[len("/ingest"):]
		},
	}
	return proxy
}
