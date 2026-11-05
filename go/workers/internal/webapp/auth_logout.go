package webapp

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/danielgtaylor/huma/v2"
)

// POST /api/auth/logout ports web/apps/web/src/app/api/auth/logout/route.ts:
// 204 with the session cookie cleared under the same name and attributes the
// sign-in set it with, and a bare 500 when the host may not carry the cookie.
// It is served only where AUTH_COOKIE_PARENT_DOMAIN is configured, so a box
// without it keeps forwarding to Next, which has it. Unlike Next it refuses a
// browser POST from another site with a bare 403, so no page can sign a user out.
const logoutPath = "/api/auth/logout"

type logoutInput struct {
	SessionInput
	ForwardedProto string `header:"X-Forwarded-Proto"`
	Origin         string `header:"Origin"`
	scheme         string
}

func (i *logoutInput) Resolve(ctx huma.Context) []error {
	i.scheme = ctx.URL().Scheme
	return i.SessionInput.Resolve(ctx)
}

type logoutOutput struct {
	Status    int
	SetCookie string `header:"Set-Cookie"`
}

func registerLogout(api huma.API) {
	domains := ParseCookieParentDomains(os.Getenv("AUTH_COOKIE_PARENT_DOMAIN"))
	if len(domains) == 0 {
		return
	}
	huma.Register(api, huma.Operation{
		OperationID:   "post-auth-logout",
		Method:        http.MethodPost,
		Path:          logoutPath,
		Summary:       "Clear the session cookie",
		DefaultStatus: http.StatusNoContent,
	}, func(_ context.Context, in *logoutInput) (*logoutOutput, error) {
		if !logoutOriginAllowed(domains, in.Origin, in.ForwardedHost, in.host) {
			return &logoutOutput{Status: http.StatusForbidden}, nil
		}
		name := privySessionCookieName(in.Cookie, in.ForwardedHost, in.host)
		cleared, err := privySessionCookieHeader(domains, name, "", -1, in.ForwardedProto, in.ForwardedHost, in.host, in.scheme)
		if err != nil {
			slog.Warn("logout cookie refused", "error", err)
			return &logoutOutput{Status: http.StatusInternalServerError}, nil
		}
		return &logoutOutput{Status: http.StatusNoContent, SetCookie: cleared}, nil
	})
}

// Browsers always send Origin on a cross-site POST; requests without one, such
// as the mobile apps', are not a CSRF vector and keep working.
func logoutOriginAllowed(domains []string, origin, forwardedHost, host string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	hostname := privyNormalizeHostname(parsed.Hostname())
	requestHost := privyCookiePrimaryValue(forwardedHost)
	if requestHost == "" {
		requestHost = host
	}
	if bare, _, err := net.SplitHostPort(requestHost); err == nil {
		requestHost = bare
	}
	if hostname == privyNormalizeHostname(requestHost) {
		return true
	}
	_, ok := privyMatchParentDomain(domains, hostname)
	return ok
}
